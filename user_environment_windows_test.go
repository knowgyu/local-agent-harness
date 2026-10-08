//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestWindowsUserEnvironmentStorePreservesStringRegistryType(t *testing.T) {
	tests := []struct {
		name         string
		exists       bool
		existingType uint32
		wantType     uint32
		wantErr      bool
	}{
		{name: "new values use literal strings", wantType: registry.SZ},
		{name: "ordinary string replacement preserves type", exists: true, existingType: registry.SZ, wantType: registry.SZ},
		{name: "expandable string replacement preserves type", exists: true, existingType: registry.EXPAND_SZ, wantType: registry.EXPAND_SZ},
		{name: "unsupported registry type fails closed", exists: true, existingType: registry.DWORD, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := currentUserEnvironmentWriteType(test.exists, test.existingType)
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected error status: got %v", err)
			}
			if err == nil && got != test.wantType {
				t.Fatalf("got registry type %d, want %d", got, test.wantType)
			}
		})
	}
}

func TestWindowsUserEnvironmentNativeSyntheticRoundTrip(t *testing.T) {
	if os.Getenv("LAH_TEST_USER_ENVIRONMENT_NATIVE") != "1" {
		t.Skip("set LAH_TEST_USER_ENVIRONMENT_NATIVE=1 to modify and clean up one synthetic HKCU environment value")
	}

	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("could not create synthetic test identity")
	}
	name := "LOCAL_AGENT_HARNESS_TEST_" + hex.EncodeToString(suffix[:])
	value := []byte("synthetic-" + hex.EncodeToString(suffix[:]))
	store := windowsUserEnvironmentStore{}
	if _, exists, err := store.ReadForUpdate(name); err != nil {
		t.Fatal("could not inspect synthetic environment variable")
	} else if exists {
		t.Skip("synthetic environment variable name already exists")
	}

	controller := newUserEnvironmentController(store, windowsUserEnvironmentChangeNotifier{})
	t.Cleanup(func() {
		_, _ = controller.DeleteUserEnvironment(context.Background(), name)
	})

	result, err := controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{Name: name, Value: value})
	if err != nil || !result.Applied {
		t.Fatal("could not save synthetic current-user environment variable")
	}
	loaded, exists, err := store.ReadForUpdate(name)
	if err != nil || !exists || !bytes.Equal(loaded, value) {
		t.Fatal("synthetic current-user environment variable did not round-trip")
	}

	deleted, err := controller.DeleteUserEnvironment(context.Background(), name)
	if err != nil || !deleted.Applied {
		t.Fatal("could not delete synthetic current-user environment variable")
	}
	if _, exists, err := store.ReadForUpdate(name); err != nil || exists {
		t.Fatal("synthetic current-user environment variable remained after deletion")
	}
}
