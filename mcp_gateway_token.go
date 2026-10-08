package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

var (
	errMCPGatewayTokenNotFound = errors.New("MCP gateway token was not found")
	errMCPGatewayTokenInvalid  = errors.New("MCP gateway token is invalid")
	errMCPGatewayTokenStore    = errors.New("MCP gateway token storage is unavailable")
)

const mcpGatewayTokenSize = 32

// mcpGatewayTokenManager keeps the reserved app credential behind the locked
// A00 provider seam. It returns raw token bytes only to the runtime owner.
type mcpGatewayTokenManager struct {
	mu       sync.Mutex
	provider gatewayTokenCredentialProvider
}

func newMCPGatewayTokenManager(provider gatewayTokenCredentialProvider) *mcpGatewayTokenManager {
	return &mcpGatewayTokenManager{provider: provider}
}

func (m *mcpGatewayTokenManager) loadOrCreate() ([]byte, error) {
	if m == nil || m.provider == nil {
		return nil, errMCPGatewayTokenStore
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadOrCreateLocked()
}

func (m *mcpGatewayTokenManager) loadOrCreateLocked() ([]byte, error) {
	token, err := m.provider.LoadGatewayToken()
	if err == nil {
		if len(token) != mcpGatewayTokenSize {
			clear(token)
			return nil, errMCPGatewayTokenInvalid
		}
		return token, nil
	}
	clear(token)
	if !errors.Is(err, errMCPGatewayTokenNotFound) {
		return nil, errMCPGatewayTokenStore
	}

	token = make([]byte, mcpGatewayTokenSize)
	if _, err := rand.Read(token); err != nil {
		clear(token)
		return nil, errMCPGatewayTokenStore
	}
	if err := m.provider.SaveGatewayToken(token); err != nil {
		clear(token)
		return nil, errMCPGatewayTokenStore
	}
	return token, nil
}

// rotate replaces the persisted token and returns private copies of the old
// and new values so the runtime owner can atomically update its verifier. The
// caller must clear both slices after use.
func (m *mcpGatewayTokenManager) rotate() (previous, next []byte, err error) {
	if m == nil || m.provider == nil {
		return nil, nil, errMCPGatewayTokenStore
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	previous, err = m.provider.LoadGatewayToken()
	if err != nil && !errors.Is(err, errMCPGatewayTokenNotFound) {
		return nil, nil, errMCPGatewayTokenStore
	}
	if err == nil && len(previous) != mcpGatewayTokenSize {
		clear(previous)
		return nil, nil, errMCPGatewayTokenInvalid
	}
	if err != nil {
		previous = nil
	}

	next = make([]byte, mcpGatewayTokenSize)
	if _, err := rand.Read(next); err != nil {
		clear(previous)
		clear(next)
		return nil, nil, errMCPGatewayTokenStore
	}
	if err := m.provider.SaveGatewayToken(next); err != nil {
		clear(previous)
		clear(next)
		return nil, nil, errMCPGatewayTokenStore
	}
	return previous, next, nil
}

func (m *mcpGatewayTokenManager) restore(token []byte) error {
	if m == nil || m.provider == nil || len(token) != mcpGatewayTokenSize {
		return errMCPGatewayTokenStore
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.provider.SaveGatewayToken(token); err != nil {
		return errMCPGatewayTokenStore
	}
	return nil
}

func (m *mcpGatewayTokenManager) revoke() error {
	if m == nil || m.provider == nil {
		return errMCPGatewayTokenStore
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.provider.DeleteGatewayToken(); err != nil && !errors.Is(err, errMCPGatewayTokenNotFound) {
		return errMCPGatewayTokenStore
	}
	return nil
}

func encodeMCPGatewayToken(token []byte) (string, error) {
	if len(token) != mcpGatewayTokenSize {
		return "", errMCPGatewayTokenInvalid
	}
	return hex.EncodeToString(token), nil
}
