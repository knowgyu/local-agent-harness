//go:build !windows

package main

import (
	"errors"
	"os"
	"sync"
)

type systemSecretStore struct{}

func credentialStoreAvailable() bool { return false }

func replaceFile(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

var configMutex sync.Mutex

func withConfigLock(fn func() error) error {
	configMutex.Lock()
	defer configMutex.Unlock()
	return fn()
}

func (systemSecretStore) Save(string, []byte) error {
	return errors.New("Windows Credential Manager is unavailable")
}
func (systemSecretStore) Load(string) ([]byte, error) {
	return nil, errors.New("Windows Credential Manager is unavailable")
}
func (systemSecretStore) Delete(string) error {
	return errors.New("Windows Credential Manager is unavailable")
}
