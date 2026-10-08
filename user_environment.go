package main

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

const maxUserEnvironmentValueUTF16Units = 32767

var (
	errUserEnvironmentUnavailable = errors.New("Could not access current-user environment settings.")
	errUserEnvironmentInvalid     = errors.New("The environment variable name or value is invalid.")
	errUserEnvironmentCollision   = errors.New("Environment variable names contain a case-insensitive collision.")
)

type userEnvironmentNameStore interface {
	ListCurrentUserNames() ([]string, error)
}

type userEnvironmentChangeNotifier interface {
	NotifyEnvironmentChanged() error
}

type userEnvironmentController struct {
	store    userEnvironmentValueStore
	names    userEnvironmentNameStore
	notifier userEnvironmentChangeNotifier
	mu       sync.Mutex
}

func newUserEnvironmentController(store userEnvironmentValueStore, notifier userEnvironmentChangeNotifier) *userEnvironmentController {
	names, _ := store.(userEnvironmentNameStore)
	return &userEnvironmentController{store: store, names: names, notifier: notifier}
}

func newDefaultUserEnvironmentController() UserEnvironmentController {
	return newPlatformUserEnvironmentController()
}

func (c *userEnvironmentController) ListUserEnvironment(ctx context.Context) ([]UserEnvironmentEntry, error) {
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.store == nil || c.names == nil {
		return nil, errUserEnvironmentUnavailable
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return nil, err
	}
	names, err := c.names.ListCurrentUserNames()
	if err != nil {
		return nil, errUserEnvironmentUnavailable
	}
	entries := make([]UserEnvironmentEntry, 0, len(names))
	seen := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		for _, prior := range seen {
			if strings.EqualFold(prior, name) {
				return nil, errUserEnvironmentCollision
			}
		}
		seen = append(seen, name)
		entries = append(entries, UserEnvironmentEntry{Name: name, Configured: true})
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if left == right {
			return entries[i].Name < entries[j].Name
		}
		return left < right
	})
	return entries, nil
}

func (c *userEnvironmentController) SetUserEnvironment(ctx context.Context, write UserEnvironmentWrite) (UserEnvironmentResult, error) {
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return UserEnvironmentResult{}, err
	}
	if err := validateUserEnvironmentWrite(write); err != nil {
		return UserEnvironmentResult{}, err
	}
	if c == nil || c.store == nil {
		return UserEnvironmentResult{}, errUserEnvironmentUnavailable
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return UserEnvironmentResult{}, err
	}
	oldValue, exists, err := c.store.ReadForUpdate(write.Name)
	if err != nil {
		return UserEnvironmentResult{}, errUserEnvironmentUnavailable
	}
	if exists && bytes.Equal(oldValue, write.Value) {
		return UserEnvironmentResult{
			Name:       write.Name,
			Operation:  "set",
			Applied:    true,
			NextAction: "No change was needed.",
		}, nil
	}
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return UserEnvironmentResult{}, err
	}
	if err := c.store.SetCurrentUserValue(write.Name, write.Value); err != nil {
		return UserEnvironmentResult{}, errUserEnvironmentUnavailable
	}
	return c.resultAfterChange(write.Name, "set"), nil
}

func (c *userEnvironmentController) DeleteUserEnvironment(ctx context.Context, name string) (UserEnvironmentResult, error) {
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return UserEnvironmentResult{}, err
	}
	if err := validateUserEnvironmentName(name); err != nil {
		return UserEnvironmentResult{}, err
	}
	if c == nil || c.store == nil {
		return UserEnvironmentResult{}, errUserEnvironmentUnavailable
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return UserEnvironmentResult{}, err
	}
	_, exists, err := c.store.ReadForUpdate(name)
	if err != nil {
		return UserEnvironmentResult{}, errUserEnvironmentUnavailable
	}
	if !exists {
		return UserEnvironmentResult{
			Name:       name,
			Operation:  "delete",
			Applied:    true,
			NextAction: "The variable was already absent.",
		}, nil
	}
	if err := checkUserEnvironmentContext(ctx); err != nil {
		return UserEnvironmentResult{}, err
	}
	if err := c.store.DeleteCurrentUserValue(name); err != nil {
		return UserEnvironmentResult{}, errUserEnvironmentUnavailable
	}
	return c.resultAfterChange(name, "delete"), nil
}

func (c *userEnvironmentController) resultAfterChange(name, operation string) UserEnvironmentResult {
	result := UserEnvironmentResult{
		Name:               name,
		Operation:          operation,
		Applied:            true,
		RequiresNewSession: true,
		NextAction:         "Close and reopen applications or sessions to receive the updated value.",
	}
	if c.notifier == nil || c.notifier.NotifyEnvironmentChanged() != nil {
		result.NextAction = "The change was saved, but Windows could not notify running applications. Reopen them; signing out and back in may be needed."
	}
	return result
}

func validateUserEnvironmentWrite(write UserEnvironmentWrite) error {
	if err := validateUserEnvironmentName(write.Name); err != nil {
		return err
	}
	if !utf8.Valid(write.Value) || bytes.IndexByte(write.Value, 0) >= 0 {
		return errUserEnvironmentInvalid
	}
	if len(utf16.Encode([]rune(string(write.Value)))) > maxUserEnvironmentValueUTF16Units {
		return errUserEnvironmentInvalid
	}
	return nil
}

func validateUserEnvironmentName(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "=\x00") {
		return errUserEnvironmentInvalid
	}
	return nil
}

func checkUserEnvironmentContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if ctx.Err() != nil {
		return errors.New("The environment variable request was canceled.")
	}
	return nil
}
