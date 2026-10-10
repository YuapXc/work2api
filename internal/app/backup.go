package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/statebackup"
)

var errBackupBusy = errors.New("backup busy")

type backupReader struct {
	ctx context.Context
	io.Reader
}

func (r backupReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type backupState struct {
	restore     sync.Once
	work        sync.Mutex
	mu          sync.Mutex
	status      string
	lastSuccess time.Time
	lastAttempt time.Time
}
type backupManifest struct {
	Version int               `json:"format_version"`
	Created string            `json:"created_at"`
	Sources map[string]string `json:"sources"`
	Files   map[string]string `json:"sha256"`
}

func (o *Orchestrator) backupStatus() map[string]any {
	o.backup.mu.Lock()
	defer o.backup.mu.Unlock()
	var last any
	if !o.backup.lastSuccess.IsZero() {
		last = o.backup.lastSuccess.Unix()
	}
	return map[string]any{"status": o.backup.status, "last_success": last}
}
func (s *Server) adminBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		writeJSON(w, 200, s.o.backupStatus())
		return
	}
	if err := s.o.runBackup(r.Context()); err != nil {
		if errors.Is(err, errBackupBusy) {
			writeAPIErr(w, errBody(409, "当前有数据操作，请稍后重试备份", "backup_busy"))
			return
		}
		log.Printf("完整备份失败: %v", err)
		writeAPIErr(w, errBody(500, "完整备份失败，请检查服务日志及已有副本", "backup_failed"))
		return
	}
	writeJSON(w, 200, s.o.backupStatus())
}
func (o *Orchestrator) scheduledBackup(ctx context.Context) {
	settings, err := o.db.GetSettings()
	if err != nil || settings["backup_enabled"] != "1" {
		return
	}
	o.backup.restore.Do(func() {
		files := completedBackups(filepath.Join(o.cfg.DataDir, "backups"))
		if len(files) == 0 {
			return
		}
		name := filepath.Base(files[len(files)-1])
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, "managed-"), ".tar.gz")
		when, e := time.Parse("20060102T150405.000000000Z", stamp)
		if e != nil {
			return
		}
		o.backup.mu.Lock()
		defer o.backup.mu.Unlock()
		if o.backup.lastSuccess.Before(when) {
			o.backup.lastSuccess = when
			o.backup.status = "complete"
		}
	})
	o.backup.mu.Lock()
	due := time.Since(o.backup.lastSuccess) >= 24*time.Hour && time.Since(o.backup.lastAttempt) >= time.Hour
	o.backup.mu.Unlock()
	if due {
		if err := o.runBackup(ctx); err != nil && !errors.Is(err, errBackupBusy) {
			log.Printf("自动完整备份失败: %v", err)
		}
	}
}
func (o *Orchestrator) runBackup(ctx context.Context) (err error) {
	if !o.backup.work.TryLock() {
		return errBackupBusy
	}
	defer o.backup.work.Unlock()
	unlock, ok := statebackup.TrySnapshot()
	if !ok {
		return errBackupBusy
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	o.backup.mu.Lock()
	o.backup.status = "running"
	o.backup.lastAttempt = time.Now()
	o.backup.mu.Unlock()
	defer func() {
		o.backup.mu.Lock()
		defer o.backup.mu.Unlock()
		if err != nil {
			o.backup.status = "failed"
		} else {
			o.backup.status = "complete"
			o.backup.lastSuccess = time.Now()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	root := filepath.Join(o.cfg.DataDir, "backups")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".pending-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	manifest := backupManifest{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), Sources: map[string]string{}, Files: map[string]string{}}
	snapshot := filepath.Join(staging, "database.sqlite")
	if err = o.db.Snapshot(ctx, snapshot); err != nil {
		return err
	}
	manifest.Sources["database.sqlite"] = o.cfg.DBPath
	snapshotInfo, err := os.Stat(snapshot)
	if err != nil {
		return err
	}
	if snapshotInfo.Size() > 1<<30 {
		return errors.New("backup database exceeds 1 GiB")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}

	roots := []string{o.cfg.DataDir, executable, filepath.Join(config.PackageRoot, ".env")}
	required := map[string]bool{}
	for _, rt := range o.runtimes.Runtimes() {
		if owner, ok := rt.(provider.StateOwner); ok {
			for _, source := range owner.StateSources() {
				if source.Path == "" {
					continue
				}
				roots = append(roots, source.Path)
				if source.Required {
					abs, e := filepath.Abs(source.Path)
					if e != nil {
						return e
					}
					required[abs] = true
				}
			}
		}
	}
	if path := os.Getenv("ENV_FILE"); path != "" {
		roots = append(roots, path)
		abs, e := filepath.Abs(path)
		if e != nil {
			return e
		}
		required[abs] = true
	}
	extra := os.Getenv("BACKUP_EXTRA_PATHS")
	if extra != "" {
		var paths []string
		if json.Unmarshal([]byte(extra), &paths) != nil {
			return fmt.Errorf("BACKUP_EXTRA_PATHS must be a JSON array")
		}
		roots = append(roots, paths...)
		for _, path := range paths {
			if strings.TrimSpace(path) == "" {
				return errors.New("empty BACKUP_EXTRA_PATHS entry")
			}
			abs, e := filepath.Abs(path)
			if e != nil {
				return e
			}
			required[abs] = true
		}
	}
	manifest.Sources["effective-config.json"] = "effective application configuration"
	config, err := json.Marshal(o.cfg)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(staging, "effective-config.json"), config, 0600); err != nil {
		return err
	}
	dbPath, _ := filepath.Abs(o.cfg.DBPath)
	seen := map[string]bool{}
	bytesCopied := snapshotInfo.Size()
	filesCopied := 0
	for _, source := range roots {
		source, err = filepath.Abs(source)
		if err != nil {
			return err
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		if _, statErr := os.Stat(source); os.IsNotExist(statErr) {
			if required[source] {
				return fmt.Errorf("required backup source missing: %s", source)
			}
			continue
		} else if statErr != nil {
			return statErr
		}
		name := fmt.Sprintf("sources/%d", len(manifest.Sources))
		manifest.Sources[name] = source
		err = filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if path == root || (path != source && (d.Name() == ".git" || d.Name() == "logs")) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if path == dbPath || path == dbPath+"-wal" || path == dbPath+"-shm" {
				return nil
			}
			if !d.IsDir() && (strings.HasSuffix(d.Name(), ".instance.lock") || d.Name() == ".instance.json") {
				return nil // Process ownership is not restorable application data.
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("snapshot source contains a symlink: %s", path)
			}
			if d.IsDir() {
				return nil
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular backup source: %s", path)
			}
			bytesCopied += info.Size()
			filesCopied++
			if filesCopied > 100000 {
				return errors.New("backup sources exceed 100000 files")
			}
			if bytesCopied > 1<<30 {
				return errors.New("backup sources exceed 1 GiB")
			}
			relative, e := filepath.Rel(source, path)
			if e != nil {
				return e
			}
			target := filepath.Join(staging, name, relative)
			if relative == "." {
				target = filepath.Join(staging, name)
			}
			if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
				return e
			}
			input, e := os.Open(path)
			if e != nil {
				return e
			}
			defer input.Close()
			output, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			n, copyErr := io.Copy(output, backupReader{ctx, io.LimitReader(input, info.Size()+1)})
			e = copyErr
			if e == nil && n != info.Size() {
				e = errors.New("backup source size changed")
			}
			if closeErr := output.Close(); e == nil {
				e = closeErr
			}
			if e != nil {
				return e
			}
			after, e := os.Stat(path)
			if e != nil {
				return e
			}
			if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				return errors.New("source changed outside snapshot coordination")
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Only the coherent capture holds the gate; hashing/compression never blocks callers.
	unlock()
	locked = false
	entries := []string{}
	err = filepath.WalkDir(staging, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		relative, e := filepath.Rel(staging, path)
		if e != nil {
			return e
		}
		file, e := os.Open(path)
		if e != nil {
			return e
		}
		hash := sha256.New()
		_, e = io.Copy(hash, backupReader{ctx, file})
		file.Close()
		if e != nil {
			return e
		}
		manifest.Files[filepath.ToSlash(relative)] = hex.EncodeToString(hash.Sum(nil))
		entries = append(entries, relative)
		return nil
	})
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(staging, "manifest.json"), raw, 0600); err != nil {
		return err
	}
	entries = append(entries, "manifest.json")
	name := "managed-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".tar.gz"
	partial := filepath.Join(root, name+".partial")
	defer os.Remove(partial)
	output, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	zip := gzip.NewWriter(io.MultiWriter(output, hash))
	archive := tar.NewWriter(zip)
	for _, relative := range entries {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		path := filepath.Join(staging, relative)
		info, e := os.Stat(path)
		if e != nil {
			err = e
			break
		}
		header, e := tar.FileInfoHeader(info, "")
		if e != nil {
			err = e
			break
		}
		header.Name = filepath.ToSlash(relative)
		header.Mode = 0600
		if e = archive.WriteHeader(header); e != nil {
			err = e
			break
		}
		file, e := os.Open(path)
		if e != nil {
			err = e
			break
		}
		_, e = io.Copy(archive, backupReader{ctx, file})
		file.Close()
		if e != nil {
			err = e
			break
		}
	}
	if e := archive.Close(); err == nil {
		err = e
	}
	if e := zip.Close(); err == nil {
		err = e
	}
	if e := output.Sync(); err == nil {
		err = e
	}
	if e := output.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	final := filepath.Join(root, name)
	if err = os.Rename(partial, final); err != nil {
		return err
	}
	marker := hex.EncodeToString(hash.Sum(nil)) + "  " + name + "\n"
	actual, err := backupDigest(final)
	if err != nil {
		return err
	}
	if actual != hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("archive read-back checksum mismatch")
	}
	markerTemp := final + ".sha256.partial"
	defer os.Remove(markerTemp)
	markerFile, err := os.OpenFile(markerTemp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = markerFile.WriteString(marker)
	if e := markerFile.Sync(); err == nil {
		err = e
	}
	if e := markerFile.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(markerTemp, final+".sha256"); err != nil {
		return err
	}
	return pruneBackups(root)
}

func backupDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("backup must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func completedBackups(root string) []string {
	files, _ := filepath.Glob(filepath.Join(root, "managed-*.tar.gz"))
	sort.Strings(files)
	complete := []string{}
	for _, file := range files {
		stamp := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "managed-"), ".tar.gz")
		if _, err := time.Parse("20060102T150405.000000000Z", stamp); err != nil {
			continue
		}
		info, err := os.Lstat(file + ".sha256")
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(64+2+len(filepath.Base(file))+1) {
			continue
		}
		data, err := os.ReadFile(file + ".sha256")
		if err != nil || len(data) != 64+2+len(filepath.Base(file))+1 {
			continue
		}
		digest, err := backupDigest(file)
		if err == nil && string(data) == digest+"  "+filepath.Base(file)+"\n" {
			complete = append(complete, file)
		}
	}
	return complete
}

func pruneBackups(root string) error {
	// Never touch manual, incomplete, or foreign files. Keep seven completed managed archives.
	complete := completedBackups(root)
	for len(complete) > 7 {
		old := complete[0]
		complete = complete[1:]
		if err := os.Remove(old); err != nil {
			return err
		}
		if err := os.Remove(old + ".sha256"); err != nil {
			return err
		}
	}
	return nil
}
