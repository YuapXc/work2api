package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchArgsPreservesControlLikeValues(t *testing.T) {
	opt, remaining := launchArgs([]string{"-admin-token", "-stop-running", "--host", "-open-browser", "-reuse-only", "--", "-build-info"})
	if opt.stop || opt.open || opt.info || !opt.reuse || strings.Join(remaining, "|") != "-admin-token|-stop-running|--host|-open-browser|--|-build-info" {
		t.Fatalf("control flags consumed configuration values: %+v %q", opt, remaining)
	}
}

func TestDrainTimeoutPreservesService(t *testing.T) {
	g := &drainGate{}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(200) }))
	go func() {
		defer close(finished)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-entered
	if err := g.drain(context.Background(), 5*time.Millisecond); err == nil {
		t.Fatal("active request bypassed")
	}
	close(release)
	<-finished
	w := httptest.NewRecorder()
	g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Fatal("old service stayed closed", w.Code)
	}
	if err := g.drain(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatal("new request entered draining service")
	}
}

func TestControlIdentityAuthorizationAndGracefulStop(t *testing.T) {
	dir := t.TempDir()
	identity := "isolated-instance"
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) }))
	defer health.Close()
	stop := make(chan struct{}, 1)
	cleanup, err := startControl(dir, identity, health.URL+"/admin-ui/", &drainGate{}, stop)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	info, err := contactInstance(dir, identity, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contactInstance(dir, "different-state", false); err == nil {
		t.Fatal("reused other state")
	}
	for _, origin := range []string{"", "http://untrusted.example"} {
		r, _ := http.NewRequest("POST", info.Control, nil)
		if origin != "" {
			r.Header.Set("Authorization", "Bearer "+info.Token)
			r.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatal("unauthorized control", resp.StatusCode)
		}
	}
	select {
	case <-stop:
		t.Fatal("unauthorized shutdown")
	default:
	}
	if _, err := contactInstance(dir, identity, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stop:
	case <-time.After(time.Second):
		t.Fatal("stop not signaled")
	}
	if _, err := contactInstance(dir, identity, false); err == nil {
		t.Fatal("draining service reused")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".instance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata instanceInfo
	if json.Unmarshal(raw, &metadata) != nil || metadata.Token == "" {
		t.Fatal("metadata invalid")
	}
}

func TestInstanceProcessLock(t *testing.T) {
	if path := os.Getenv("WORK2API_TEST_LOCK"); path != "" {
		release, err := lockInstance([]string{path})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		fmt.Println("locked")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	path := filepath.Join(t.TempDir(), "state.instance.lock")
	child := exec.Command(os.Args[0], "-test.run=^TestInstanceProcessLock$")
	child.Env = append(os.Environ(), "WORK2API_TEST_LOCK="+path)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("child lock failed: %q %v", line, err)
	}
	if release, err := lockInstance([]string{path}); err == nil {
		release()
		t.Fatal("shared state permitted")
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	release, err := lockInstance([]string{path})
	if err != nil {
		t.Fatal("crash did not release lock", err)
	}
	defer release()
	other, err := lockInstance([]string{filepath.Join(t.TempDir(), "separate.instance.lock")})
	if err != nil {
		t.Fatal(err)
	}
	other()
}

// TestListenExplicitPortOccupiedFailsLoud: a port the operator set explicitly
// (i.e. not the default 8787) must NOT be auto-avoided — listen returns an
// error so their choice is respected instead of silently drifting.
func TestListenExplicitPortOccupiedFailsLoud(t *testing.T) {
	// Occupy an OS-assigned free port, then demand exactly that port.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup listen: %v", err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port
	if port == 8787 { // astronomically unlikely, but keep the test meaningful
		t.Skip("OS handed us the default port")
	}

	ln, _, err := listen("127.0.0.1", port)
	if err == nil {
		ln.Close()
		t.Fatalf("explicit occupied port %d should fail, not avoid", port)
	}
	if !strings.Contains(err.Error(), "已被占用") {
		t.Fatalf("error should explain the port is occupied, got: %v", err)
	}
}

// TestListenBindsFreePort: a free port binds and reports its address back.
func TestListenBindsFreePort(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close() // release it so listen() can grab the same port

	ln, addr, err := listen("127.0.0.1", port)
	if err != nil {
		t.Fatalf("free port %d should bind, got: %v", port, err)
	}
	defer ln.Close()
	if !strings.HasSuffix(addr, ":"+itoa(port)) {
		t.Fatalf("addr %q should end in bound port %d", addr, port)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
