//go:build !windows

package main

import (
	"errors"
	"syscall"
)

func isResidentEndpointConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
