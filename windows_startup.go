package main

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"unicode/utf16"
)

const (
	loginStartupDisabled    loginStartupState = "disabled"
	loginStartupEnabled     loginStartupState = "enabled"
	loginStartupConflict    loginStartupState = "conflict"
	loginStartupUnavailable loginStartupState = "unavailable"

	windowsRunCommandMaxUTF16Units = 260
)

var (
	errLoginStartupUnavailable = errors.New("login startup settings are unavailable")
	errLoginStartupConflict    = errors.New("a different login startup entry uses the reserved name")
	errLoginStartupCommand     = errors.New("login startup command is invalid")
)

type loginStartupState string

type loginStartupValueStore interface {
	Read() (value string, exists bool, err error)
	Write(value string) error
	Delete() error
}

type loginStartupControl interface {
	State() loginStartupState
	Enable() error
	Disable() error
}

type managedLoginStartupController struct {
	mu      sync.Mutex
	store   loginStartupValueStore
	command string
}

func newManagedLoginStartupController(store loginStartupValueStore, command string) (*managedLoginStartupController, error) {
	if store == nil || len(command) == 0 || strings.ContainsAny(command, "\x00\r\n") ||
		len(utf16.Encode([]rune(command))) > windowsRunCommandMaxUTF16Units {
		return nil, errLoginStartupCommand
	}
	return &managedLoginStartupController{store: store, command: command}, nil
}

func (c *managedLoginStartupController) State() loginStartupState {
	if c == nil || c.store == nil {
		return loginStartupUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value, exists, err := c.store.Read()
	if err != nil {
		return loginStartupUnavailable
	}
	if !exists {
		return loginStartupDisabled
	}
	if value == c.command {
		return loginStartupEnabled
	}
	return loginStartupConflict
}

func (c *managedLoginStartupController) Enable() error {
	if c == nil || c.store == nil {
		return errLoginStartupUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value, exists, err := c.store.Read()
	if err != nil {
		return errLoginStartupUnavailable
	}
	if exists {
		if value == c.command {
			return nil
		}
		return errLoginStartupConflict
	}
	if err := c.store.Write(c.command); err != nil {
		return errLoginStartupUnavailable
	}
	return nil
}

func (c *managedLoginStartupController) Disable() error {
	if c == nil || c.store == nil {
		return errLoginStartupUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value, exists, err := c.store.Read()
	if err != nil {
		return errLoginStartupUnavailable
	}
	if !exists {
		return nil
	}
	if value != c.command {
		return errLoginStartupConflict
	}
	if err := c.store.Delete(); err != nil {
		return errLoginStartupUnavailable
	}
	return nil
}

type fixedLoginStartupController struct {
	state loginStartupState
	err   error
}

func (c fixedLoginStartupController) State() loginStartupState { return c.state }
func (c fixedLoginStartupController) Enable() error            { return c.err }
func (c fixedLoginStartupController) Disable() error           { return c.err }

func (a *app) handleLoginStartup(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	if a.loginStartup == nil {
		a.setStatus("Windows login startup is unavailable on this platform.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var err error
	switch r.FormValue("action") {
	case "enable":
		err = a.loginStartup.Enable()
		if err == nil {
			a.setStatus("Resident MCP will start when you sign in to this Windows account.", false)
		}
	case "disable":
		err = a.loginStartup.Disable()
		if err == nil {
			a.setStatus("Resident MCP will no longer start automatically at sign-in.", false)
		}
	default:
		a.setStatus("Login startup settings were not changed.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, errLoginStartupConflict):
			a.setStatus("A different login startup entry uses the reserved name. It was left unchanged.", true)
		default:
			a.setStatus("Could not update Windows login startup. Existing settings were left unchanged.", true)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
