//go:build windows

package main

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type systemSecretStore struct{}

type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32    = syscall.NewLazyDLL("advapi32.dll")
	credWriteW  = advapi32.NewProc("CredWriteW")
	credReadW   = advapi32.NewProc("CredReadW")
	credDeleteW = advapi32.NewProc("CredDeleteW")
	credFree    = advapi32.NewProc("CredFree")
)

func credentialStoreAvailable() bool { return true }

func replaceFile(oldpath, newpath string) error {
	return windows.Rename(oldpath, newpath)
}

func withConfigLock(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	name, err := windows.UTF16PtrFromString(`Local\LocalAgentHarness.Config.v1`)
	if err != nil {
		return errors.New("Could not create the settings lock.")
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if handle == 0 || (err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS)) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return errors.New("Could not access the settings lock.")
	}
	defer windows.CloseHandle(handle)
	wait, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil || (wait != windows.WAIT_OBJECT_0 && wait != windows.WAIT_ABANDONED) {
		return errors.New("Could not acquire the settings lock.")
	}
	defer windows.ReleaseMutex(handle)
	return fn()
}

func (systemSecretStore) Save(ref string, value []byte) error {
	if !secretRefPattern.MatchString(ref) || len(value) == 0 || len(value) > maxSecretSize {
		return errors.New("invalid credential")
	}
	target, err := syscall.UTF16PtrFromString("LocalAgentHarness/" + ref)
	if err != nil {
		return err
	}
	user, _ := syscall.UTF16PtrFromString("Local Agent Harness secret")
	c := credentialW{Type: 1, TargetName: target, CredentialBlobSize: uint32(len(value)), Persist: 2, UserName: user}
	if len(value) > 0 {
		c.CredentialBlob = &value[0]
	}
	result, _, callErr := credWriteW.Call(uintptr(unsafe.Pointer(&c)), 0)
	if result == 0 {
		return callErr
	}
	return nil
}

func (systemSecretStore) Load(ref string) ([]byte, error) {
	if !secretRefPattern.MatchString(ref) {
		return nil, errors.New("invalid credential reference")
	}
	target, err := syscall.UTF16PtrFromString("LocalAgentHarness/" + ref)
	if err != nil {
		return nil, err
	}
	var ptr *credentialW
	result, _, callErr := credReadW.Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&ptr)))
	if result == 0 {
		return nil, callErr
	}
	defer credFree.Call(uintptr(unsafe.Pointer(ptr)))
	if ptr == nil || ptr.CredentialBlobSize == 0 || ptr.CredentialBlobSize > maxSecretSize {
		return nil, errors.New("invalid credential data")
	}
	return append([]byte(nil), unsafe.Slice(ptr.CredentialBlob, int(ptr.CredentialBlobSize))...), nil
}

func (systemSecretStore) Delete(ref string) error {
	if !secretRefPattern.MatchString(ref) {
		return errors.New("invalid credential reference")
	}
	target, err := syscall.UTF16PtrFromString("LocalAgentHarness/" + ref)
	if err != nil {
		return err
	}
	result, _, callErr := credDeleteW.Call(uintptr(unsafe.Pointer(target)), 1, 0)
	return normalizeCredentialDeleteResult(result, callErr)
}

func normalizeCredentialDeleteResult(result uintptr, callErr error) error {
	if result != 0 || errors.Is(callErr, windows.ERROR_NOT_FOUND) {
		return nil
	}
	if callErr == nil {
		return errors.New("credential deletion failed")
	}
	return callErr
}
