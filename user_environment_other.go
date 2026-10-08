//go:build !windows

package main

import "errors"

type unsupportedUserEnvironmentStore struct{}

func newPlatformUserEnvironmentController() UserEnvironmentController {
	return newUserEnvironmentController(unsupportedUserEnvironmentStore{}, nil)
}

func (unsupportedUserEnvironmentStore) ListCurrentUserNames() ([]string, error) {
	return nil, errors.New("Current-user environment management is available on Windows only.")
}

func (unsupportedUserEnvironmentStore) ReadForUpdate(string) ([]byte, bool, error) {
	return nil, false, errors.New("Current-user environment management is available on Windows only.")
}

func (unsupportedUserEnvironmentStore) SetCurrentUserValue(string, []byte) error {
	return errors.New("Current-user environment management is available on Windows only.")
}

func (unsupportedUserEnvironmentStore) DeleteCurrentUserValue(string) error {
	return errors.New("Current-user environment management is available on Windows only.")
}
