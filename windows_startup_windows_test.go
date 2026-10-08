//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManualWindowsLoginStartupSyntheticRegistryAcceptance(t *testing.T) {
	if os.Getenv("LAH_ENABLE_MANUAL_WINDOWS_STARTUP_ACCEPTANCE") != "1" {
		t.Skip("set LAH_ENABLE_MANUAL_WINDOWS_STARTUP_ACCEPTANCE=1 to write and remove one synthetic current-user Run value")
	}

	var randomName [16]byte
	if _, err := rand.Read(randomName[:]); err != nil {
		t.Fatal(err)
	}
	store := windowsRunLoginStartupStore{valueName: "LocalAgentHarnessTest-" + hex.EncodeToString(randomName[:])}
	if _, exists, err := store.Read(); err != nil || exists {
		t.Fatalf("synthetic registry value preflight failed: exists=%t err=%v", exists, err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	command := windows.EscapeArg(executable) + " " + residentBackgroundArg
	controller, err := newManagedLoginStartupController(store, command)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		value, exists, readErr := store.Read()
		if readErr != nil || !exists || value != command {
			if readErr != nil || exists {
				t.Errorf("synthetic startup value was not safe to clean up: exists=%t readErr=%v", exists, readErr)
			}
			return
		}
		if err := controller.Disable(); err != nil {
			t.Errorf("synthetic startup cleanup failed: %v", err)
		}
	})

	if err := controller.Enable(); err != nil {
		t.Fatal(err)
	}
	if got := controller.State(); got != loginStartupEnabled {
		t.Fatalf("synthetic value state after enable = %q, want enabled", got)
	}
	if err := controller.Disable(); err != nil {
		t.Fatal(err)
	}
	if got := controller.State(); got != loginStartupDisabled {
		t.Fatalf("synthetic value state after disable = %q, want disabled", got)
	}
}
