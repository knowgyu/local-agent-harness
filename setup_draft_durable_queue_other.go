//go:build !windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func withSetupDraftQueueLock(path string, fn func() error) error {
	if fn == nil || ensureSetupDraftQueueDirectory(path) != nil {
		return errSetupDraftQueueUnavailable
	}
	lockPath := path + ".lock"
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	lockFile := os.NewFile(uintptr(fd), lockPath)
	if lockFile == nil {
		_ = syscall.Close(fd)
		return errSetupDraftQueueUnavailable
	}
	defer lockFile.Close()
	if !validSetupDraftQueueUnixFile(lockFile, true) || syscall.Flock(fd, syscall.LOCK_EX) != nil {
		return errSetupDraftQueueUnavailable
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return fn()
}

func ensureSetupDraftQueueDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errSetupDraftQueueUnavailable
	}
	directory := filepath.Dir(filepath.Clean(path))
	parent := filepath.Dir(directory)
	if err := rejectSetupDraftQueueUnixSymlinkComponents(parent); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return errSetupDraftQueueUnavailable
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || !validSetupDraftQueueUnixOwner(info) {
		return errSetupDraftQueueUnavailable
	}
	return nil
}

func rejectSetupDraftQueueUnixSymlinkComponents(path string) error {
	volume := filepath.VolumeName(path)
	root := volume + string(filepath.Separator)
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return errSetupDraftQueueUnavailable
	}
	current := root
	for _, component := range splitSetupDraftQueuePath(relative) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errSetupDraftQueueUnavailable
		}
	}
	return nil
}

func splitSetupDraftQueuePath(path string) []string {
	return strings.Split(path, string(filepath.Separator))
}

func readSetupDraftQueueFile(path string, maxBytes int64) ([]byte, error) {
	if ensureSetupDraftQueueDirectory(path) != nil {
		return nil, errSetupDraftQueueUnavailable
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, errSetupDraftQueueUnavailable
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errSetupDraftQueueUnavailable
	}
	defer file.Close()
	if !validSetupDraftQueueUnixFile(file, false) {
		return nil, errSetupDraftQueueUnavailable
	}
	info, err := file.Stat()
	if err != nil || info.Size() == 0 || info.Size() > maxBytes {
		return nil, errSetupDraftQueueInvalid
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) != info.Size() || int64(len(data)) > maxBytes {
		return nil, errSetupDraftQueueInvalid
	}
	return data, nil
}

func writeSetupDraftQueueFileAtomically(path string, data []byte) error {
	if ensureSetupDraftQueueDirectory(path) != nil {
		return errSetupDraftQueueUnavailable
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errSetupDraftQueueUnavailable
	}
	tempPath := path + "." + hex.EncodeToString(random[:]) + ".tmp"
	fd, err := syscall.Open(tempPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	file := os.NewFile(uintptr(fd), tempPath)
	if file == nil {
		_ = syscall.Close(fd)
		_ = os.Remove(tempPath)
		return errSetupDraftQueueUnavailable
	}
	keep := false
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if !validSetupDraftQueueUnixFile(file, true) || writeFullQueueData(file, data) != nil || file.Sync() != nil {
		return errSetupDraftQueueUnavailable
	}
	if err := file.Close(); err != nil {
		return errSetupDraftQueueUnavailable
	}
	file = nil
	if err := os.Rename(tempPath, path); err != nil {
		return errSetupDraftQueueUnavailable
	}
	keep = true
	if _, err := readSetupDraftQueueFile(path, setupDraftQueueMaxFileBytes); err != nil {
		return errSetupDraftQueueUnavailable
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errSetupDraftQueueUnavailable
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errSetupDraftQueueUnavailable
	}
	return nil
}

func validSetupDraftQueueUnixFile(file *os.File, allowWrite bool) bool {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 || !validSetupDraftQueueUnixOwner(info) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return false
	}
	if allowWrite && info.Mode().Perm()&0200 == 0 {
		return false
	}
	return true
}

func validSetupDraftQueueUnixOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
