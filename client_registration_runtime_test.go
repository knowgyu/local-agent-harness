package main

import (
	"context"
	"errors"
	"testing"
)

type runtimeTestBackupStore struct{}

func (runtimeTestBackupStore) save(context.Context, string, clientRegistrationConfig, clientRegistrationDigest) (MCPClientBackupReceipt, error) {
	return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
}

func (runtimeTestBackupStore) load(context.Context, string, string) (clientRegistrationBackupRecord, error) {
	return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
}

func (runtimeTestBackupStore) findLatest(context.Context, string, clientRegistrationDigest) (clientRegistrationBackupRecord, error) {
	return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
}

func (runtimeTestBackupStore) prepareApply(context.Context, string, string, clientRegistrationConfig, clientRegistrationDigest) error {
	return errClientRegistrationBackupRequired
}

func (runtimeTestBackupStore) markApplied(context.Context, string, string) error {
	return errClientRegistrationBackupRequired
}

func (runtimeTestBackupStore) markRestored(context.Context, string, string) error {
	return errClientRegistrationBackupRequired
}

func TestProductionRegistrationAdapterAssemblyUsesAllThreeClients(t *testing.T) {
	adapters, err := newProductionClientRegistrationAdapters()
	if err != nil {
		t.Fatalf("assemble production adapters: %v", err)
	}
	if len(adapters) != 3 {
		t.Fatalf("production adapter count = %d, want 3", len(adapters))
	}
	want := []string{mcpClientIDCodex, mcpClientIDClaude, mcpClientIDGemini}
	for index, adapter := range adapters {
		if adapter == nil || adapter.clientID() != want[index] {
			t.Fatalf("production adapter %d has unexpected client ID", index)
		}
	}
	controller, err := newClientRegistrationControllerWithAdapters(adapters, runtimeTestBackupStore{})
	if err != nil || controller == nil {
		t.Fatalf("construct controller with injected store: controller=%v err=%v", controller, err)
	}
}

func TestInjectedRegistrationControllerRequiresFakeBackupStore(t *testing.T) {
	_, err := newClientRegistrationControllerWithAdapters(nil, nil)
	if !errors.Is(err, errClientRegistrationControllerOptions) {
		t.Fatalf("nil injected store error = %v", err)
	}
}
