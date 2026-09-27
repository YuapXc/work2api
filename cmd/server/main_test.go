package main

import (
	"net"
	"strings"
	"testing"
)

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
