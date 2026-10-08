//go:build windows

package main

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNormalizeCredentialDeleteResultTreatsNotFoundAsIdempotentSuccess(t *testing.T) {
	if err := normalizeCredentialDeleteResult(1, errors.New("ignored success last-error")); err != nil {
		t.Fatalf("successful delete result = %v", err)
	}
	if err := normalizeCredentialDeleteResult(0, fmt.Errorf("wrapped: %w", windows.ERROR_NOT_FOUND)); err != nil {
		t.Fatalf("already-absent credential should be idempotent: %v", err)
	}
	other := errors.New("synthetic credential store failure")
	if err := normalizeCredentialDeleteResult(0, other); !errors.Is(err, other) {
		t.Fatalf("other delete failure was suppressed: %v", err)
	}
	if err := normalizeCredentialDeleteResult(0, nil); err == nil {
		t.Fatal("zero result without a last-error was treated as success")
	}
}
