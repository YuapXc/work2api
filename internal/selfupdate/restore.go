package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func NoSymlinks(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := absolute; ; current = filepath.Dir(current) {
		info, e := os.Lstat(current)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("更新路径包含符号链接")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func ReplaceFile(from, to string, mode os.FileMode) error {
	if err := NoSymlinks(to); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(to), ".work2api-replace-")
	if err != nil {
		return err
	}
	name := temp.Name()
	temp.Close()
	os.Remove(name)
	defer os.Remove(name)
	if err = CopyFile(from, name, mode); err != nil {
		return err
	}
	if err = os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, to)
}

type restoreManifest struct {
	Version int               `json:"format_version"`
	Sources map[string]string `json:"sources"`
	Files   map[string]string `json:"sha256"`
}

// The snapshot generator and reader share this manifest budget, independently
// of small control/plan JSON files. Large account pools legitimately exceed 1MiB.
const MaxBackupManifestBytes = 64 << 20

// Validate every byte before writing live paths. Restore only a locally pinned
// coherent backup, while both instances are stopped and data locks are held.
func RestoreBackup(archive, digest, target, data, db string) error {
	return restoreBackup(archive, digest, target, data, db, false)
}

// ValidateBackup exercises the same archive, digest and path checks without
// touching live state. Run it before the old service leaves its update gate.
func ValidateBackup(archive, digest, target, data, db string) error {
	return restoreBackup(archive, digest, target, data, db, true)
}

func restoreBackup(archive, digest, target, data, db string, validateOnly bool) error {
	if !ValidHash(digest) {
		return errors.New("备份摘要无效")
	}
	actual, err := HashFile(archive)
	if err != nil || actual != digest {
		return errors.New("更新备份校验失败")
	}
	staging, err := os.MkdirTemp(filepath.Dir(archive), "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	hashes := map[string]string{}
	var manifest restoreManifest
	var total int64
	for {
		header, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name := header.Name
		if header.Typeflag != tar.TypeReg || header.Size < 0 || strings.Contains(name, "\\") || strings.Contains(name, ":") || filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name || strings.HasPrefix(name, "../") {
			return errors.New("备份条目无效")
		}
		total += header.Size
		if total > 1<<30 || len(hashes) > 100000 {
			return errors.New("备份恢复超出保护范围")
		}
		if _, exists := hashes[name]; exists {
			return errors.New("备份包含重复条目")
		}
		destination := filepath.Join(staging, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return err
		}
		output, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(output, hash), reader)
		closeErr := output.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if n != header.Size {
			return errors.New("备份条目长度不符")
		}
		hashes[name] = hex.EncodeToString(hash.Sum(nil))
	}
	manifestFile, err := os.Open(filepath.Join(staging, "manifest.json"))
	if err != nil {
		return err
	}
	info, err := manifestFile.Stat()
	if err != nil || info.Size() > MaxBackupManifestBytes {
		manifestFile.Close()
		return errors.New("备份清单超过允许大小")
	}
	decoder := json.NewDecoder(io.LimitReader(manifestFile, MaxBackupManifestBytes+1))
	err = decoder.Decode(&manifest)
	if err == nil {
		var extra any
		if e := decoder.Decode(&extra); e != io.EOF {
			err = errors.New("备份清单含多余数据")
		}
	}
	manifestFile.Close()
	if err != nil {
		return err
	}
	if manifest.Version != 1 || manifest.Sources["database.sqlite"] != db {
		return errors.New("备份数据库路径不匹配")
	}
	if len(hashes) != len(manifest.Files)+1 {
		return errors.New("备份清单条目不匹配")
	}
	for name, expected := range manifest.Files {
		if hashes[name] != expected || !ValidHash(expected) {
			return errors.New("备份内容摘要不匹配")
		}
	}
	targets := map[string]string{}
	for name := range manifest.Files {
		if name == "effective-config.json" {
			continue
		}
		destination := ""
		if name == "database.sqlite" {
			destination = db
		} else {
			for prefix, source := range manifest.Sources {
				if !strings.HasPrefix(prefix, "sources/") {
					continue
				}
				if name == prefix {
					destination = source
					break
				}
				if strings.HasPrefix(name, prefix+"/") {
					relative := strings.TrimPrefix(name, prefix+"/")
					destination = filepath.Join(source, filepath.FromSlash(relative))
					break
				}
			}
		}
		if destination == "" || !filepath.IsAbs(destination) {
			return errors.New("无法映射备份状态路径")
		}
		if destination == target {
			continue
		}
		if err = NoSymlinks(destination); err != nil {
			return err
		}
		if previous, exists := targets[destination]; exists {
			relative, _ := filepath.Rel(staging, previous)
			if hashes[filepath.ToSlash(relative)] != hashes[name] {
				return errors.New("备份状态源重叠且内容不一致")
			}
		}
		targets[destination] = filepath.Join(staging, filepath.FromSlash(name))
	}
	if validateOnly {
		return nil
	}
	// Startup may create files after a migration. Remove extra service-owned data
	// only, preserving locks, logs, backup archives and the updater itself.
	if err = filepath.WalkDir(data, func(path string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if path == data {
			return nil
		}
		relative, _ := filepath.Rel(data, path)
		first := strings.Split(filepath.ToSlash(relative), "/")[0]
		if first == ".updates" || first == ".usage-exports" || first == "backups" || first == "logs" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("恢复数据目录含符号链接")
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() == ".instance.json" || strings.HasSuffix(entry.Name(), ".instance.lock") {
			return nil
		}
		if path == db+"-wal" || path == db+"-shm" {
			return os.Remove(path)
		}
		if _, kept := targets[path]; !kept {
			return os.Remove(path)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = os.Remove(db + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	paths := []string{}
	for path := range targets {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if info, e := os.Stat(path); e == nil {
			mode = info.Mode().Perm()
		}
		if err = ReplaceFile(targets[path], path, mode); err != nil {
			return err
		}
	}
	return nil
}
