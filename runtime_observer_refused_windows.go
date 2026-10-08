//go:build windows

package main

import (
	"errors"
	"syscall"
)

// Winsock reports WSAECONNREFUSED as 10061; syscall.ECONNREFUSED is a
// synthetic errno value on Windows and does not match the native dial error.
const windowsWSAECONNREFUSED = syscall.Errno(10061)

func isResidentEndpointConnectionRefused(err error) bool {
	return errors.Is(err, windowsWSAECONNREFUSED)
}
