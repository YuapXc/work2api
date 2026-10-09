package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Locks remain on disk: removing a locked file can let another process lock
// a different inode. The OS releases ownership even after an abrupt exit.
func canonicalLockPaths(paths []string) ([]string, error) {
	unique := map[string]bool{}
	for _, path := range paths {
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		path = filepath.Join(parent, filepath.Base(path))
		if runtime.GOOS == "windows" {
			path = strings.ToLower(path)
		}
		unique[path] = true
	}
	ordered := make([]string, 0, len(unique))
	for path := range unique {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return ordered, nil
}

func lockInstance(paths []string) (func(), error) {
	ordered, err := canonicalLockPaths(paths)
	if err != nil {
		return nil, err
	}
	var files []*os.File
	release := func() {
		for i := len(files) - 1; i >= 0; i-- {
			_ = files[i].Close()
		}
	}
	for _, path := range ordered {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			release()
			return nil, err
		}
		if err := lockFile(f); err != nil {
			_ = f.Close()
			release()
			return nil, fmt.Errorf("数据或凭据已被其他实例占用（%s）；请使用现有实例或独立的数据和凭据目录：%w", strings.TrimSuffix(path, ".instance.lock"), err)
		}
		files = append(files, f)
	}
	return release, nil
}
