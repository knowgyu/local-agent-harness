//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

func resolveOpenSSHExecutable() (string, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return "", errors.New("The local OpenSSH client was not found.")
	}
	return path, nil
}
