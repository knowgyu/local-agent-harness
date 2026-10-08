//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type systemMCPGatewayTokenCredentialProvider struct{}

func newSystemMCPGatewayTokenCredentialProvider() gatewayTokenCredentialProvider {
	return systemMCPGatewayTokenCredentialProvider{}
}

func (systemMCPGatewayTokenCredentialProvider) LoadGatewayToken() ([]byte, error) {
	return loadMCPGatewayTokenCredential(gatewayCredentialTargetName)
}

func loadMCPGatewayTokenCredential(targetName string) ([]byte, error) {
	target, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return nil, errMCPGatewayTokenStore
	}
	var credential *credentialW
	result, _, callErr := credReadW.Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&credential)))
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil, errMCPGatewayTokenNotFound
		}
		return nil, errMCPGatewayTokenStore
	}
	if credential == nil {
		return nil, errMCPGatewayTokenInvalid
	}
	defer credFree.Call(uintptr(unsafe.Pointer(credential)))
	if credential.CredentialBlob == nil || credential.CredentialBlobSize != mcpGatewayTokenSize {
		return nil, errMCPGatewayTokenInvalid
	}
	return append([]byte(nil), unsafe.Slice(credential.CredentialBlob, int(credential.CredentialBlobSize))...), nil
}

func (systemMCPGatewayTokenCredentialProvider) SaveGatewayToken(token []byte) error {
	return saveMCPGatewayTokenCredential(gatewayCredentialTargetName, token)
}

func saveMCPGatewayTokenCredential(targetName string, token []byte) error {
	if len(token) != mcpGatewayTokenSize {
		return errMCPGatewayTokenInvalid
	}
	target, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return errMCPGatewayTokenStore
	}
	user, err := syscall.UTF16PtrFromString("Local Agent Harness MCP gateway")
	if err != nil {
		return errMCPGatewayTokenStore
	}
	credential := credentialW{
		Type:               1,
		TargetName:         target,
		CredentialBlobSize: uint32(len(token)),
		CredentialBlob:     &token[0],
		Persist:            2,
		UserName:           user,
	}
	result, _, _ := credWriteW.Call(uintptr(unsafe.Pointer(&credential)), 0)
	if result == 0 {
		return errMCPGatewayTokenStore
	}
	return nil
}

func (systemMCPGatewayTokenCredentialProvider) DeleteGatewayToken() error {
	return deleteMCPGatewayTokenCredential(gatewayCredentialTargetName)
}

func deleteMCPGatewayTokenCredential(targetName string) error {
	target, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return errMCPGatewayTokenStore
	}
	result, _, callErr := credDeleteW.Call(uintptr(unsafe.Pointer(target)), 1, 0)
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return errMCPGatewayTokenNotFound
		}
		return errMCPGatewayTokenStore
	}
	return nil
}
