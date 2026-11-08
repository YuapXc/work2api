// Package selfupdate stages verified official releases. Installation is delegated
// to an independent helper so process exit cannot interrupt recovery.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Build struct {
	Application string `json:"application"`
	Version     string `json:"version"`
	Release     bool   `json:"release_build"`
	Repository  string `json:"repository"`
	Commit      string `json:"commit"`
	Schema      int    `json:"schema"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Build       string `json:"build"`
}
type Deployment struct {
	Kind       string `json:"kind"`
	CanInstall bool   `json:"can_install"`
	Reason     string `json:"reason,omitempty"`
	Unit       string `json:"-"`
}
type Policy struct {
	Mode             string `json:"mode"`
	EveryHours       int    `json:"every_hours"`
	StartHour        int    `json:"start_hour"`
	EndHour          int    `json:"end_hour"`
	AllowRepublished bool   `json:"allow_republished"`
}
type Asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}
type Release struct {
	Tag        string  `json:"tag_name"`
	URL        string  `json:"html_url"`
	Notes      string  `json:"body"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}
type Candidate struct {
	Build      Build  `json:"build"`
	Digest     string `json:"digest"`
	BinaryHash string `json:"binary_hash"`
	AssetID    int64  `json:"asset_id"`
	Migration  bool   `json:"migration"`
	Path       string `json:"-"`
}
type Status struct {
	Current     Build      `json:"current"`
	Deployment  Deployment `json:"deployment"`
	Policy      Policy     `json:"policy"`
	Phase       string     `json:"phase"`
	Message     string     `json:"message"`
	Latest      *Release   `json:"latest,omitempty"`
	Candidate   *Candidate `json:"candidate,omitempty"`
	LastCheck   *time.Time `json:"last_check,omitempty"`
	Available   bool       `json:"available"`
	SameVersion bool       `json:"same_version"`
	Timezone    string     `json:"timezone"`
}

// Plan is local-only and private: neither arguments nor runtime control secrets
// are returned by the admin API. A random transaction directory owns all files.
type Plan struct {
	BindHost      string     `json:"bind_host"`
	BindPort      int        `json:"bind_port"`
	CandidatePath string     `json:"candidate_path"`
	ID            string     `json:"id"`
	Token         string     `json:"token"`
	Root          string     `json:"root"`
	Target        string     `json:"target"`
	OldHash       string     `json:"old_hash"`
	Candidate     Candidate  `json:"candidate"`
	Current       Build      `json:"current"`
	Deployment    Deployment `json:"deployment"`
	Unit          string     `json:"unit"`
	DataDir       string     `json:"data_dir"`
	DBPath        string     `json:"db_path"`
	CWD           string     `json:"cwd"`
	Args          []string   `json:"args"`
	Backup        string     `json:"backup"`
	BackupHash    string     `json:"backup_hash"`
	Identity      string     `json:"identity"`
	Phase         string     `json:"phase"`
	Message       string     `json:"message"`
	Started       time.Time  `json:"started"`
}
type Manager struct {
	failedAt     time.Time
	mu           sync.Mutex
	busy         bool
	root, target string
	status       Status
	staged       *Candidate
	launch       func(string) error
	makePlan     func() Plan
}

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func New(root, target string, current Build, deployment Deployment, launch func(string) error, makePlan func() Plan) *Manager {
	m := &Manager{root: root, target: target, launch: launch, makePlan: makePlan, status: Status{Current: current, Deployment: deployment, Phase: "idle", Timezone: time.Now().Format("MST -07:00"), Policy: Policy{Mode: "off", EveryHours: 6, StartHour: 2, EndHour: 4}}}
	var p Policy
	if ReadJSON(filepath.Join(root, "policy.json"), &p) == nil && ValidPolicy(p) == nil {
		m.status.Policy = p
	}
	var staged Plan
	if deployment.CanInstall && ReadJSON(filepath.Join(root, "staged.json"), &staged) == nil {
		c := staged.Candidate
		owned := filepath.Dir(filepath.Dir(c.Path)) == root && strings.HasPrefix(filepath.Base(filepath.Dir(c.Path)), "candidate-")
		if owned && c.Build.Release && c.Build.Repository == current.Repository && c.Build.OS == runtime.GOOS && c.Build.Arch == runtime.GOARCH && c.Build.Schema >= current.Schema && compareVersion(c.Build.Version, current.Version) >= 0 && NoSymlinks(c.Path) == nil {
			if hash, e := HashFile(c.Path); e == nil && hash == c.BinaryHash {
				installed, _ := HashFile(target)
				if installed == hash {
					_ = os.Remove(filepath.Join(root, "staged.json"))
					_ = os.RemoveAll(filepath.Dir(c.Path))
				} else {
					m.staged = &c
					m.status.Candidate = &c
					m.status.Available = true
					m.status.Phase = "ready"
				}
			}
		}
	}
	return m
}
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.status
	var result struct{ Phase, Message string }
	if ReadJSON(filepath.Join(m.root, "result.json"), &result) == nil && result.Phase != "" {
		out.Phase = result.Phase
		out.Message = result.Message
	}
	return out
}
func ValidPolicy(p Policy) error {
	if p.Mode != "off" && p.Mode != "check" && p.Mode != "auto" {
		return errors.New("未知更新模式")
	}
	if p.EveryHours < 1 || p.EveryHours > 168 || p.StartHour < 0 || p.StartHour > 23 || p.EndHour < 0 || p.EndHour > 23 || p.StartHour == p.EndHour {
		return errors.New("检查间隔或维护时段无效")
	}
	return nil
}
func (m *Manager) SetPolicy(p Policy) error {
	if err := ValidPolicy(p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy {
		return errors.New("更新任务正在执行")
	}
	if p.Mode == "auto" && !m.status.Deployment.CanInstall {
		return errors.New(m.status.Deployment.Reason)
	}
	if err := WriteJSON(filepath.Join(m.root, "policy.json"), p); err != nil {
		return err
	}
	m.status.Policy = p
	return nil
}
func (m *Manager) begin(phase string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy {
		return errors.New("已有更新任务正在执行")
	}
	var active Plan
	if ReadJSON(filepath.Join(m.root, "pending.json"), &active) == nil && (active.Phase == "installing" || active.Phase == "backup_ready" || active.Phase == "verifying" || active.Phase == "accepting" || active.Phase == "acceptance_unknown" || active.Phase == "recovery_required") {
		return errors.New("已有安装事务，请先处理其结果")
	}
	m.busy = true
	m.status.Phase = phase
	m.status.Message = ""
	_ = os.Remove(filepath.Join(m.root, "result.json"))
	return nil
}
func (m *Manager) end(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = false
	if err != nil {
		m.status.Phase = "failed"
		m.status.Message = err.Error()
		m.failedAt = time.Now()
	}
}
func (m *Manager) Check(ctx context.Context) (err error) {
	if err = m.begin("checking"); err != nil {
		return
	}
	defer func() { m.end(err) }()
	m.mu.Lock()
	checkTime := time.Now()
	m.status.LastCheck = &checkTime
	m.mu.Unlock()
	repo := m.status.Current.Repository
	if !repoPattern.MatchString(repo) {
		return errors.New("构建缺少可信更新仓库标识")
	}
	raw, e := fetch(ctx, "https://api.github.com/repos/"+repo+"/releases/latest", 2<<20)
	if e != nil {
		return e
	}
	var release Release
	if json.Unmarshal(raw, &release) != nil || !tagPattern.MatchString(release.Tag) || release.Draft || release.Prerelease {
		return errors.New("没有可用稳定版 Release")
	}
	release.URL = "https://github.com/" + repo + "/releases/tag/" + release.Tag
	if len(release.Notes) > 32768 {
		release.Notes = release.Notes[:32768]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.status.LastCheck = &now
	m.status.Latest = &release
	m.status.SameVersion = release.Tag == m.status.Current.Version
	m.status.Available = compareVersion(release.Tag, m.status.Current.Version) >= 0
	if m.staged != nil && m.staged.Build.Version != release.Tag {
		_ = os.RemoveAll(filepath.Dir(m.staged.Path))
		_ = os.Remove(filepath.Join(m.root, "staged.json"))
		m.staged = nil
		m.status.Candidate = nil
	}
	m.status.Phase = "checked"
	return nil
}
func compareVersion(a, b string) int {
	if !tagPattern.MatchString(b) {
		return 1
	}
	for i, x := range strings.Split(strings.TrimPrefix(a, "v"), ".") {
		av, _ := strconv.ParseUint(x, 10, 64)
		bv, _ := strconv.ParseUint(strings.Split(strings.TrimPrefix(b, "v"), ".")[i], 10, 64)
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	return 0
}
func (m *Manager) Download(ctx context.Context) (err error) {
	if err = m.begin("downloading"); err != nil {
		return
	}
	defer func() { m.end(err) }()
	m.mu.Lock()
	latest := m.status.Latest
	current := m.status.Current
	deployment := m.status.Deployment
	m.mu.Unlock()
	if !deployment.CanInstall {
		return errors.New(deployment.Reason)
	}
	if latest == nil || compareVersion(latest.Tag, current.Version) < 0 {
		return errors.New("请先检查最新稳定版，自动更新不支持降级")
	}
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	name := fmt.Sprintf("work2api_%s_%s_%s.%s", latest.Tag, runtime.GOOS, runtime.GOARCH, ext)
	var asset, checksum *Asset
	for i := range latest.Assets {
		a := &latest.Assets[i]
		if a.Name == name {
			asset = a
		}
		if a.Name == name+".sha256" {
			checksum = a
		}
	}
	if asset == nil || checksum == nil || asset.Size <= 0 || asset.Size > 128<<20 {
		return errors.New("Release 尚未包含匹配平台的完整产物和校验文件")
	}
	if !assetURL(asset.URL, current.Repository, latest.Tag) || !assetURL(checksum.URL, current.Repository, latest.Tag) {
		return errors.New("Release 下载地址不属于当前发布仓库")
	}
	if e := os.MkdirAll(m.root, 0700); e != nil {
		return e
	}
	sum, e := downloadChecksum(ctx, *checksum, current.Repository, latest.Tag, m.root)
	if e != nil {
		return e
	}
	parts := strings.Fields(string(sum))
	if len(parts) != 2 || strings.TrimPrefix(parts[1], "*") != name || !ValidHash(parts[0]) {
		return errors.New("Release 校验文件无效")
	}
	digest := strings.ToLower(parts[0])
	if asset.Digest != "" && asset.Digest != "sha256:"+digest {
		return errors.New("GitHub资产摘要与校验文件不一致")
	}
	dir, e := os.MkdirTemp(m.root, "candidate-")
	if os.IsNotExist(e) {
		if e = os.MkdirAll(m.root, 0700); e == nil {
			dir, e = os.MkdirTemp(m.root, "candidate-")
		}
	}
	if e != nil {
		return e
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	archive := filepath.Join(dir, "release.archive")
	if e = downloadReleaseAsset(ctx, *asset, current.Repository, latest.Tag, archive, 128<<20); e != nil {
		return e
	}
	actual, e := hashFileContext(ctx, archive)
	if e != nil {
		return e
	}
	if actual != digest {
		return errors.New("下载产物SHA-256不匹配")
	}
	path := filepath.Join(dir, ExecutableName())
	if e = extractBinaryFileContext(ctx, archive, ext, path); e != nil {
		return e
	}
	if e = os.Remove(archive); e != nil {
		return e
	}
	probe, e := Probe(ctx, path)
	if e != nil {
		return e
	}
	if !probe.Release || probe.Application != "work2api" || probe.Repository != current.Repository || probe.OS != runtime.GOOS || probe.Arch != runtime.GOARCH || probe.Version != latest.Tag || probe.Schema < current.Schema {
		return errors.New("候选程序来源、平台、版本或数据库兼容性不匹配")
	}
	binaryHash, e := hashFileContext(ctx, path)
	if e != nil {
		return e
	}
	oldHash, e := hashFileContext(ctx, m.target)
	if e != nil {
		return e
	}
	candidate := &Candidate{Build: probe, Digest: digest, BinaryHash: binaryHash, AssetID: asset.ID, Migration: probe.Schema > current.Schema, Path: path}
	if err := ctx.Err(); err != nil {
		return err
	}
	if oldHash != binaryHash {
		if e = WriteJSON(filepath.Join(m.root, "staged.json"), Plan{Candidate: *candidate, CandidatePath: candidate.Path}); e != nil {
			return e
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if oldHash == binaryHash {
		m.status.Available = false
		m.status.Phase = "current"
		m.status.Message = "已安装相同产物"
		return nil
	}
	if m.staged != nil {
		os.RemoveAll(filepath.Dir(m.staged.Path))
	}
	m.staged = candidate
	m.status.Candidate = candidate
	m.status.Phase = "ready"
	keep = true
	return nil
}
func (m *Manager) Install(allowMigration bool, expectedDigest ...string) (err error) {
	if err = m.begin("installing"); err != nil {
		return
	}
	defer func() { m.end(err) }()
	m.mu.Lock()
	candidate := m.staged
	deployment := m.status.Deployment
	m.mu.Unlock()
	if !deployment.CanInstall {
		return errors.New(deployment.Reason)
	}
	if candidate == nil {
		return errors.New("请先下载并验证更新")
	}
	if len(expectedDigest) > 0 && expectedDigest[0] != candidate.Digest {
		return errors.New("已验证产物发生变化，请刷新状态后重新确认")
	}
	if candidate.Migration && !allowMigration {
		return errors.New("此版本包含数据库迁移，需要手动确认安装")
	}
	if hash, e := HashFile(candidate.Path); e != nil || hash != candidate.BinaryHash {
		return errors.New("候选文件已改变，请重新下载")
	}
	dir, e := os.MkdirTemp(m.root, "transaction-")
	if e != nil {
		return e
	}
	var random [32]byte
	if _, e = rand.Read(random[:]); e != nil {
		return e
	}
	plan := m.makePlan()
	plan.ID = filepath.Base(dir)
	plan.Root = dir
	plan.Token = hex.EncodeToString(random[:])
	plan.Candidate = *candidate
	plan.CandidatePath = candidate.Path
	plan.Current = m.status.Current
	plan.Deployment = deployment
	plan.Unit = deployment.Unit
	plan.Target = m.target
	plan.OldHash, e = HashFile(m.target)
	if e != nil {
		return e
	}
	plan.Phase = "installing"
	plan.Started = time.Now()
	// Keep a private helper copy outside the target path; Windows locks live EXEs.
	helper := filepath.Join(dir, "helper"+filepath.Ext(ExecutableName()))
	if e = CopyFile(m.target, helper, 0700); e != nil {
		return e
	}
	if e = WriteJSON(filepath.Join(dir, "plan.json"), plan); e != nil {
		return e
	}
	if e = WriteJSON(filepath.Join(m.root, "pending.json"), plan); e != nil {
		return e
	}
	if e = WriteJSON(filepath.Join(m.root, "attempt.json"), map[string]any{"version": candidate.Build.Version, "digest": candidate.Digest, "time": time.Now()}); e != nil {
		return e
	}
	if e = m.launch(filepath.Join(dir, "plan.json")); e != nil {
		plan.Phase = "failed"
		plan.Message = "更新助手启动失败，旧服务未停止"
		_ = WriteJSON(filepath.Join(m.root, "pending.json"), plan)
		return e
	}
	m.mu.Lock()
	m.status.Message = "更新助手已启动；等待现有调用结束"
	m.mu.Unlock()
	return nil
}
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			backoff := !m.failedAt.IsZero() && time.Since(m.failedAt) < time.Hour
			m.mu.Unlock()
			if backoff {
				continue
			}
			s := m.Status()
			if s.Policy.Mode == "off" {
				continue
			}
			if s.LastCheck == nil || now.Sub(*s.LastCheck) >= time.Duration(s.Policy.EveryHours)*time.Hour {
				checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_ = m.Check(checkCtx)
				cancel()
			}
			s = m.Status()
			if s.Policy.Mode != "auto" || !s.Available || !s.Deployment.CanInstall || !inWindow(now.Hour(), s.Policy.StartHour, s.Policy.EndHour) || s.SameVersion && !s.Policy.AllowRepublished {
				continue
			}
			if s.Candidate == nil {
				downloadCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				_ = m.Download(downloadCtx)
				cancel()
			}
			s = m.Status()
			if s.Candidate != nil && !s.Candidate.Migration && !m.recentAttempt(*s.Candidate) {
				_ = m.Install(false)
			}
		}
	}
}
func inWindow(hour, start, end int) bool {
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}
func assetURL(raw, repo, tag string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && strings.HasPrefix(u.Path, "/"+repo+"/releases/download/"+tag+"/")
}
func fetch(ctx context.Context, raw string, limit int64) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "work2api-updater")
	resp, e := githubClient().Do(req)
	if e != nil {
		return nil, errors.New("连接GitHub更新源失败，请稍后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub更新源返回HTTP %d", resp.StatusCode)
	}
	rawData, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(rawData)) > limit {
		return nil, errors.New("更新响应超出大小保护")
	}
	return rawData, e
}

func githubClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 5 || r.URL.Scheme != "https" || r.URL.User != nil {
			return errors.New("更新下载重定向无效")
		}
		host := r.URL.Hostname()
		if host != "github.com" && host != "api.github.com" && !strings.HasSuffix(host, ".githubusercontent.com") {
			return errors.New("更新下载跳转到未知来源")
		}
		return nil
	}}
}
func ExecutableName() string {
	if runtime.GOOS == "windows" {
		return "work2api.exe"
	}
	return "work2api"
}
func ValidHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}
func HashFile(path string) (string, error) {
	return hashFileContext(context.Background(), path)
}

func hashFileContext(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer file.Close()
	h := sha256.New()
	_, e = io.Copy(h, contextReader{ctx, file})
	if e == nil {
		e = ctx.Err()
	}
	return hex.EncodeToString(h.Sum(nil)), e
}
func CopyFile(from, to string, mode os.FileMode) error {
	input, e := os.Open(from)
	if e != nil {
		return e
	}
	defer input.Close()
	output, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	_, e = io.Copy(output, input)
	if e == nil {
		e = output.Sync()
	}
	if closeErr := output.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		os.Remove(to)
	}
	return e
}
func WriteJSON(path string, value any) error {
	if e := NoSymlinks(filepath.Dir(path)); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	raw, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	temp, e := os.CreateTemp(filepath.Dir(path), ".json-")
	if e != nil {
		return e
	}
	defer os.Remove(temp.Name())
	_ = temp.Chmod(0600)
	if _, e = temp.Write(raw); e == nil {
		e = temp.Sync()
	}
	if closeErr := temp.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(temp.Name(), path)
}
func ReadJSON(path string, value any) error {
	file, e := os.Open(path)
	if e != nil {
		return e
	}
	defer file.Close()
	err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(value)
	if plan, ok := value.(*Plan); ok && err == nil {
		plan.Candidate.Path = plan.CandidatePath
	}
	return err
}
func Probe(ctx context.Context, path string) (Build, error) {
	var info Build
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "-build-info")
	prepareProbe(command)
	raw, e := command.Output()
	if e != nil || len(raw) > 4096 || json.Unmarshal(raw, &info) != nil {
		return info, errors.New("候选程序版本检查失败")
	}
	return info, nil
}
func ExtractBinary(data []byte, format, target string) error {
	return extractBinary(context.Background(), bytes.NewReader(data), int64(len(data)), format, target)
}

func ExtractBinaryFile(path, format, target string) error {
	return extractBinaryFileContext(context.Background(), path, format, target)
}

func extractBinaryFileContext(ctx context.Context, path, format, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return extractBinary(ctx, f, info.Size(), format, target)
}

type archiveSource interface {
	io.Reader
	io.ReaderAt
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type contextArchive struct {
	contextReader
	source io.ReaderAt
}

func (r contextArchive) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.ReadAt(p, off)
}

func extractBinary(ctx context.Context, data archiveSource, length int64, format, target string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	data = contextArchive{contextReader{ctx, data}, data}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	count := 0
	accept := func(name string, size int64, reader io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clean := strings.ReplaceAll(name, "\\", "/")
		if strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") || strings.Contains(clean, "../") {
			return errors.New("更新包包含无效路径")
		}
		if filepath.Base(clean) != ExecutableName() {
			return nil
		}
		count++
		if count != 1 || size <= 0 || size > 128<<20 {
			return errors.New("更新包程序数量或大小无效")
		}
		var e error
		n, e := io.Copy(f, io.LimitReader(contextReader{ctx, reader}, size+1))
		if e != nil {
			return e
		}
		if n != size {
			return errors.New("更新包程序长度不符")
		}
		return e
	}
	if format == "zip" {
		r, e := zip.NewReader(data, length)
		if e != nil {
			return e
		}
		for _, f := range r.File {
			if err := ctx.Err(); err != nil {
				return err
			}
			if f.Mode()&os.ModeSymlink != 0 {
				return errors.New("更新包包含符号链接")
			}
			if f.FileInfo().IsDir() {
				continue
			}
			reader, e := f.Open()
			if e != nil {
				return e
			}
			e = accept(f.Name, int64(f.UncompressedSize64), reader)
			reader.Close()
			if e != nil {
				return e
			}
		}
	} else {
		gz, e := gzip.NewReader(data)
		if e != nil {
			return e
		}
		defer gz.Close()
		r := tar.NewReader(gz)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			h, e := r.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			if h.Typeflag == tar.TypeDir {
				continue
			}
			if h.Typeflag != tar.TypeReg {
				return errors.New("更新包包含特殊文件")
			}
			if e = accept(h.Name, h.Size, r); e != nil {
				return e
			}
		}
	}
	if count != 1 {
		return errors.New("更新包缺少程序")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return ctx.Err()
}

// Prepare runs only after the old HTTP gate is drained. Pin the backup outside
// normal retention so the helper can restore the exact pre-update state.
func (m *Manager) Prepare(ctx context.Context, token string, backup func(context.Context) (string, error)) error {
	var plan Plan
	if err := ReadJSON(filepath.Join(m.root, "pending.json"), &plan); err != nil {
		return err
	}
	if plan.Phase != "installing" || plan.Token != token || filepath.Dir(plan.Root) != m.root {
		return errors.New("更新事务无效")
	}
	if digest, e := HashFile(m.target); e != nil || digest != plan.OldHash {
		return errors.New("当前程序已改变")
	}
	source, err := backup(ctx)
	if err != nil {
		return err
	}
	plan.Backup = filepath.Join(plan.Root, "rollback.tar.gz")
	if err = CopyFile(source, plan.Backup, 0600); err != nil {
		return err
	}
	plan.BackupHash, err = HashFile(plan.Backup)
	if err != nil {
		return err
	}
	if err = ValidateBackup(plan.Backup, plan.BackupHash, plan.Target, plan.DataDir, plan.DBPath); err != nil {
		return fmt.Errorf("更新前备份不可恢复: %w", err)
	}
	plan.Phase = "backup_ready"
	if err = WriteJSON(filepath.Join(plan.Root, "plan.json"), plan); err != nil {
		return err
	}
	return WriteJSON(filepath.Join(m.root, "pending.json"), plan)
}

func (m *Manager) ValidateOriginal(token string) error {
	var plan Plan
	if err := ReadJSON(filepath.Join(m.root, "pending.json"), &plan); err != nil {
		return err
	}
	if plan.Token != token {
		return errors.New("事务不匹配")
	}
	if hash, err := HashFile(m.target); err != nil || hash != plan.OldHash {
		return errors.New("原程序已改变")
	}
	return nil
}

func (m *Manager) recentAttempt(candidate Candidate) bool {
	var previous struct {
		Version string    `json:"version"`
		Digest  string    `json:"digest"`
		Time    time.Time `json:"time"`
	}
	if ReadJSON(filepath.Join(m.root, "attempt.json"), &previous) != nil {
		return false
	}
	return previous.Version == candidate.Build.Version && previous.Digest == candidate.Digest && time.Since(previous.Time) < 24*time.Hour
}
