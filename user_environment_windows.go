//go:build windows

package main

import (
	"errors"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const currentUserEnvironmentRegistryPath = `Environment`

type windowsUserEnvironmentStore struct{}

type windowsUserEnvironmentChangeNotifier struct{}

var (
	user32DLL                 = syscall.NewLazyDLL("user32.dll")
	sendMessageTimeoutW       = user32DLL.NewProc("SendMessageTimeoutW")
	errWindowsEnvironmentRead = errors.New("Could not read current-user environment settings.")
)

func newPlatformUserEnvironmentController() UserEnvironmentController {
	store := windowsUserEnvironmentStore{}
	return newUserEnvironmentController(store, windowsUserEnvironmentChangeNotifier{})
}

func (windowsUserEnvironmentStore) ListCurrentUserNames() ([]string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, currentUserEnvironmentRegistryPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, errWindowsEnvironmentRead
	}
	defer key.Close()
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return nil, errWindowsEnvironmentRead
	}
	return names, nil
}

func (windowsUserEnvironmentStore) ReadForUpdate(name string) ([]byte, bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, currentUserEnvironmentRegistryPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errWindowsEnvironmentRead
	}
	defer key.Close()

	canonicalName, exists, err := findCurrentUserEnvironmentName(key, name)
	if err != nil || !exists {
		return nil, exists, err
	}
	value, _, err := key.GetStringValue(canonicalName)
	if err != nil {
		return nil, false, errWindowsEnvironmentRead
	}
	return []byte(value), true, nil
}

func (windowsUserEnvironmentStore) SetCurrentUserValue(name string, value []byte) error {
	if !utf8.Valid(value) || strings.ContainsRune(string(value), '\x00') {
		return errors.New("Could not save current-user environment settings.")
	}
	key, _, err := registry.CreateKey(
		registry.CURRENT_USER,
		currentUserEnvironmentRegistryPath,
		registry.QUERY_VALUE|registry.SET_VALUE,
	)
	if err != nil {
		return errors.New("Could not save current-user environment settings.")
	}
	defer key.Close()

	canonicalName, exists, err := findCurrentUserEnvironmentName(key, name)
	if err != nil {
		return errors.New("Could not save current-user environment settings.")
	}
	var existingType uint32
	if exists {
		_, existingType, err = key.GetStringValue(canonicalName)
		if err != nil {
			return errors.New("Could not replace current-user environment settings.")
		}
	} else {
		canonicalName = name
	}
	valueType, err := currentUserEnvironmentWriteType(exists, existingType)
	if err != nil {
		return err
	}

	var setErr error
	switch valueType {
	case registry.SZ:
		setErr = key.SetStringValue(canonicalName, string(value))
	case registry.EXPAND_SZ:
		setErr = key.SetExpandStringValue(canonicalName, string(value))
	default:
		return errors.New("The existing environment value has an unsupported type.")
	}
	if setErr != nil {
		return errors.New("Could not save current-user environment settings.")
	}
	return nil
}

func currentUserEnvironmentWriteType(exists bool, existingType uint32) (uint32, error) {
	if !exists {
		return registry.SZ, nil
	}
	switch existingType {
	case registry.SZ, registry.EXPAND_SZ:
		return existingType, nil
	default:
		return 0, errors.New("The existing environment value has an unsupported type.")
	}
}

func (windowsUserEnvironmentStore) DeleteCurrentUserValue(name string) error {
	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		currentUserEnvironmentRegistryPath,
		registry.QUERY_VALUE|registry.SET_VALUE,
	)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Could not delete current-user environment settings.")
	}
	defer key.Close()

	canonicalName, exists, err := findCurrentUserEnvironmentName(key, name)
	if err != nil {
		return errors.New("Could not delete current-user environment settings.")
	}
	if !exists {
		return nil
	}
	if err := key.DeleteValue(canonicalName); err != nil {
		return errors.New("Could not delete current-user environment settings.")
	}
	return nil
}

func findCurrentUserEnvironmentName(key registry.Key, requested string) (string, bool, error) {
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return "", false, errWindowsEnvironmentRead
	}
	matched := ""
	for _, name := range names {
		if !strings.EqualFold(name, requested) {
			continue
		}
		if matched != "" {
			return "", false, errUserEnvironmentCollision
		}
		matched = name
	}
	return matched, matched != "", nil
}

func (windowsUserEnvironmentChangeNotifier) NotifyEnvironmentChanged() error {
	environment, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return errors.New("Could not notify running applications about environment changes.")
	}
	var receiverResult uintptr
	result, _, _ := sendMessageTimeoutW.Call(
		uintptr(0xFFFF), // HWND_BROADCAST
		uintptr(0x001A), // WM_SETTINGCHANGE
		0,
		uintptr(unsafe.Pointer(environment)),
		uintptr(0x0002), // SMTO_ABORTIFHUNG
		uintptr(1000),
		uintptr(unsafe.Pointer(&receiverResult)),
	)
	if result == 0 {
		return errors.New("Could not notify running applications about environment changes.")
	}
	return nil
}
