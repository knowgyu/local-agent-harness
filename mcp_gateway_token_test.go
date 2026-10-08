package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeMCPGatewayTokenProvider struct {
	mu        sync.Mutex
	token     []byte
	loadErr   error
	saveErr   error
	deleteErr error
	saves     int
	loads     int
	deletes   int
}

func (p *fakeMCPGatewayTokenProvider) LoadGatewayToken() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loads++
	if p.loadErr != nil {
		return nil, p.loadErr
	}
	if len(p.token) == 0 {
		return nil, errMCPGatewayTokenNotFound
	}
	return append([]byte(nil), p.token...), nil
}

func (p *fakeMCPGatewayTokenProvider) SaveGatewayToken(token []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saves++
	if p.saveErr != nil {
		return p.saveErr
	}
	if len(token) != mcpGatewayTokenSize {
		return errMCPGatewayTokenInvalid
	}
	clear(p.token)
	p.token = append([]byte(nil), token...)
	return nil
}

func (p *fakeMCPGatewayTokenProvider) DeleteGatewayToken() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes++
	if p.deleteErr != nil {
		return p.deleteErr
	}
	if len(p.token) == 0 {
		return errMCPGatewayTokenNotFound
	}
	clear(p.token)
	p.token = nil
	return nil
}

func (p *fakeMCPGatewayTokenProvider) snapshot() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.token...)
}

func TestMCPGatewayTokenManagerProvisionReuseRotateAndRevoke(t *testing.T) {
	provider := &fakeMCPGatewayTokenProvider{}
	manager := newMCPGatewayTokenManager(provider)
	first, err := manager.loadOrCreate()
	if err != nil {
		t.Fatal("loadOrCreate failed")
	}
	if len(first) != mcpGatewayTokenSize || !bytes.Equal(first, provider.snapshot()) {
		t.Fatal("provisioned gateway token has invalid length or was not persisted")
	}
	second, err := manager.loadOrCreate()
	if err != nil {
		t.Fatal("reload of provisioned gateway token failed")
	}
	if !bytes.Equal(first, second) {
		t.Fatal("gateway token changed across reload")
	}
	previous, rotated, err := manager.rotate()
	if err != nil {
		t.Fatal("gateway token rotation failed")
	}
	if !bytes.Equal(previous, second) || bytes.Equal(rotated, previous) || !bytes.Equal(rotated, provider.snapshot()) {
		t.Fatal("rotation did not replace the stored gateway token")
	}
	clear(first)
	clear(second)
	clear(previous)
	clear(rotated)
	if err := manager.revoke(); err != nil {
		t.Fatal("gateway token revoke failed")
	}
	if len(provider.snapshot()) != 0 {
		t.Fatal("gateway token remained in the provider after revoke")
	}
	if err := manager.revoke(); err != nil {
		t.Fatal("revoking an absent gateway token was not idempotent")
	}
}

func TestMCPGatewayTokenManagerRejectsCorruptionAndStoreErrors(t *testing.T) {
	provider := &fakeMCPGatewayTokenProvider{token: []byte("short")}
	manager := newMCPGatewayTokenManager(provider)
	if _, err := manager.loadOrCreate(); !errors.Is(err, errMCPGatewayTokenInvalid) {
		t.Fatalf("loadOrCreate error = %v, want invalid-token error", err)
	}
	if provider.saves != 0 {
		t.Fatal("corrupt token was silently replaced")
	}

	provider = &fakeMCPGatewayTokenProvider{loadErr: errors.New("private store detail")}
	manager = newMCPGatewayTokenManager(provider)
	if _, err := manager.loadOrCreate(); !errors.Is(err, errMCPGatewayTokenStore) {
		t.Fatalf("loadOrCreate error = %v, want generic storage error", err)
	}
	if strings.Contains(errMCPGatewayTokenStore.Error(), "private store detail") {
		t.Fatal("storage error exposed provider details")
	}

	provider = &fakeMCPGatewayTokenProvider{saveErr: errors.New("private store detail")}
	manager = newMCPGatewayTokenManager(provider)
	if _, err := manager.loadOrCreate(); !errors.Is(err, errMCPGatewayTokenStore) {
		t.Fatalf("initial save error = %v, want generic storage error", err)
	}
	if len(provider.snapshot()) != 0 {
		t.Fatal("failed token provisioning left a token in the provider")
	}
}

func TestMCPGatewayTokenManagerRejectsInvalidEncodingInput(t *testing.T) {
	for _, value := range [][]byte{nil, make([]byte, mcpGatewayTokenSize-1), make([]byte, mcpGatewayTokenSize+1)} {
		if _, err := encodeMCPGatewayToken(value); !errors.Is(err, errMCPGatewayTokenInvalid) {
			t.Errorf("encodeMCPGatewayToken accepted %d bytes", len(value))
		}
	}
	if _, err := newMCPGatewayTokenManager(nil).loadOrCreate(); !errors.Is(err, errMCPGatewayTokenStore) {
		t.Fatal("nil provider did not fail closed")
	}
}
