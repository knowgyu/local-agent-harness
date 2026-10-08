//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	windowsLoginRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	windowsLoginRunValue   = "LocalAgentHarnessResidentMCP"
	residentBackgroundArg  = "--resident --background"
)

type windowsRunLoginStartupStore struct {
	valueName string
}

func newSystemLoginStartupController() loginStartupControl {
	executable, err := os.Executable()
	if err != nil {
		return fixedLoginStartupController{state: loginStartupUnavailable, err: errLoginStartupUnavailable}
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fixedLoginStartupController{state: loginStartupUnavailable, err: errLoginStartupUnavailable}
	}
	command := windows.EscapeArg(executable) + " " + residentBackgroundArg
	controller, err := newManagedLoginStartupController(
		windowsRunLoginStartupStore{valueName: windowsLoginRunValue},
		command,
	)
	if err != nil {
		return fixedLoginStartupController{state: loginStartupUnavailable, err: errLoginStartupUnavailable}
	}
	return controller
}

func (s windowsRunLoginStartupStore) Read() (string, bool, error) {
	if s.valueName == "" {
		return "", false, errLoginStartupUnavailable
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsLoginRunKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errLoginStartupUnavailable
	}
	defer key.Close()
	value, valueType, err := key.GetStringValue(s.valueName)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil || valueType != registry.SZ {
		return "", true, errLoginStartupUnavailable
	}
	return value, true, nil
}

func (s windowsRunLoginStartupStore) Write(value string) error {
	if s.valueName == "" || value == "" {
		return errLoginStartupUnavailable
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, windowsLoginRunKeyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return errLoginStartupUnavailable
	}
	defer key.Close()
	if err := key.SetStringValue(s.valueName, value); err != nil {
		return errLoginStartupUnavailable
	}
	return nil
}

func (s windowsRunLoginStartupStore) Delete() error {
	if s.valueName == "" {
		return errLoginStartupUnavailable
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsLoginRunKeyPath, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errLoginStartupUnavailable
	}
	defer key.Close()
	if err := key.DeleteValue(s.valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return errLoginStartupUnavailable
	}
	return nil
}

func hideBackgroundConsole() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	window, _, _ := getConsoleWindow.Call()
	if window == 0 {
		return
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	showWindow := user32.NewProc("ShowWindow")
	_, _, _ = showWindow.Call(window, uintptr(windows.SW_HIDE))
}
