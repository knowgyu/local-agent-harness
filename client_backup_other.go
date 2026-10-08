//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

func ensureClientRegistrationBackupDirectory(path string) error {
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return validateClientRegistrationBackupStoragePath(path, true)
}

func secureClientRegistrationBackupFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return validateClientRegistrationBackupStoragePath(path, false)
}

func validateClientRegistrationBackupStoragePath(path string, directory bool) error {
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return os.ErrPermission
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return os.ErrPermission
	}
	return nil
}

func openClientRegistrationBackupFile(path string) (*os.File, error) {
	if err := validateClientRegistrationBackupStoragePath(path, false); err != nil {
		return nil, err
	}
	return os.Open(path)
}
