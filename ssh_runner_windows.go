//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

func resolveOpenSSHExecutable() (string, error) {
	if windowsDirectory := os.Getenv("WINDIR"); windowsDirectory != "" {
		candidate := filepath.Join(windowsDirectory, "System32", "OpenSSH", "ssh.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	path, err := exec.LookPath("ssh.exe")
	if err != nil {
		return "", errors.New("The local OpenSSH client was not found.")
	}
	return path, nil
}
