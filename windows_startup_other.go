//go:build !windows

package main

func newSystemLoginStartupController() loginStartupControl {
	return fixedLoginStartupController{state: loginStartupUnavailable, err: errLoginStartupUnavailable}
}

func hideBackgroundConsole() {}
