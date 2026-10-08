package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeUserEnvironmentValue struct {
	name  string
	value []byte
}

type fakeUserEnvironmentStore struct {
	values    map[string]fakeUserEnvironmentValue
	readErr   error
	setErr    error
	deleteErr error
	readCalls int
	setCalls  int
	delCalls  int
}

func newFakeUserEnvironmentStore(entries ...fakeUserEnvironmentValue) *fakeUserEnvironmentStore {
	store := &fakeUserEnvironmentStore{values: make(map[string]fakeUserEnvironmentValue)}
	for _, entry := range entries {
		store.values[strings.ToLower(entry.name)] = fakeUserEnvironmentValue{
			name:  entry.name,
			value: append([]byte(nil), entry.value...),
		}
	}
	return store
}

func (s *fakeUserEnvironmentStore) ListCurrentUserNames() ([]string, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	names := make([]string, 0, len(s.values))
	for _, entry := range s.values {
		names = append(names, entry.name)
	}
	return names, nil
}

func (s *fakeUserEnvironmentStore) ReadForUpdate(name string) ([]byte, bool, error) {
	s.readCalls++
	if s.readErr != nil {
		return nil, false, s.readErr
	}
	entry, ok := s.values[strings.ToLower(name)]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), entry.value...), true, nil
}

func (s *fakeUserEnvironmentStore) SetCurrentUserValue(name string, value []byte) error {
	s.setCalls++
	if s.setErr != nil {
		return s.setErr
	}
	key := strings.ToLower(name)
	if existing, ok := s.values[key]; ok {
		name = existing.name
	}
	s.values[key] = fakeUserEnvironmentValue{name: name, value: append([]byte(nil), value...)}
	return nil
}

func (s *fakeUserEnvironmentStore) DeleteCurrentUserValue(name string) error {
	s.delCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.values, strings.ToLower(name))
	return nil
}

type fakeUserEnvironmentNotifier struct {
	err   error
	calls int
}

func (n *fakeUserEnvironmentNotifier) NotifyEnvironmentChanged() error {
	n.calls++
	return n.err
}

func TestUserEnvironmentListReturnsOnlyNamesAndConfiguredState(t *testing.T) {
	store := newFakeUserEnvironmentStore(
		fakeUserEnvironmentValue{name: "ZED", value: []byte("secret-z")},
		fakeUserEnvironmentValue{name: "alpha", value: []byte("secret-a")},
	)
	controller := newUserEnvironmentController(store, &fakeUserEnvironmentNotifier{})
	entries, err := controller.ListUserEnvironment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "alpha" || entries[1].Name != "ZED" {
		t.Fatalf("unexpected sorted names: %#v", entries)
	}
	for _, entry := range entries {
		if !entry.Configured {
			t.Fatalf("listed value should be marked configured: %#v", entry)
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-") {
		t.Fatalf("list response included a stored value: %s", encoded)
	}
}

func TestUserEnvironmentSetCreateAndReplacePreserveOtherValuesAndCase(t *testing.T) {
	store := newFakeUserEnvironmentStore(
		fakeUserEnvironmentValue{name: "KeepThis", value: []byte("keep-canary")},
		fakeUserEnvironmentValue{name: "ExistingName", value: []byte("old-canary")},
	)
	notifier := &fakeUserEnvironmentNotifier{}
	controller := newUserEnvironmentController(store, notifier)

	created, err := controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{
		Name:  "NEW_NAME",
		Value: []byte("new-canary"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Applied || !created.RequiresNewSession || created.Operation != "set" {
		t.Fatalf("unexpected create result: %#v", created)
	}
	if _, ok := store.values["keepthis"]; !ok || string(store.values["keepthis"].value) != "keep-canary" {
		t.Fatal("unselected variable was changed")
	}

	replaced, err := controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{
		Name:  "existingname",
		Value: []byte("replacement-canary"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replaced.Applied || replaced.Name != "existingname" {
		t.Fatalf("unexpected replace result: %#v", replaced)
	}
	entry := store.values["existingname"]
	if entry.name != "ExistingName" || string(entry.value) != "replacement-canary" {
		t.Fatalf("case-insensitive replacement did not preserve the existing name: %#v", entry)
	}
	if notifier.calls != 2 {
		t.Fatalf("expected notification after each write, got %d", notifier.calls)
	}
	for _, text := range []string{created.NextAction, replaced.NextAction} {
		if strings.Contains(text, "canary") {
			t.Fatalf("result exposed a value: %q", text)
		}
	}
}

func TestUserEnvironmentSetEmptyValueIsConfiguredAndSameValueIsNoOp(t *testing.T) {
	store := newFakeUserEnvironmentStore()
	notifier := &fakeUserEnvironmentNotifier{}
	controller := newUserEnvironmentController(store, notifier)

	result, err := controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{Name: "EMPTY_OK"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || !result.RequiresNewSession {
		t.Fatalf("empty value should be saved as a configured value: %#v", result)
	}
	entries, err := controller.ListUserEnvironment(context.Background())
	if err != nil || len(entries) != 1 || entries[0].Name != "EMPTY_OK" || !entries[0].Configured {
		t.Fatalf("empty value was not listed as configured: entries=%#v err=%v", entries, err)
	}

	result, err = controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{Name: "EMPTY_OK"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.RequiresNewSession || result.NextAction != "No change was needed." {
		t.Fatalf("identical update should not require a new session: %#v", result)
	}
	if notifier.calls != 1 || store.setCalls != 1 {
		t.Fatalf("no-op update repeated backend work: notifications=%d writes=%d", notifier.calls, store.setCalls)
	}
}

func TestUserEnvironmentDeleteIsCaseInsensitiveAndPreservesOtherValues(t *testing.T) {
	store := newFakeUserEnvironmentStore(
		fakeUserEnvironmentValue{name: "DeleteMe", value: []byte("delete-canary")},
		fakeUserEnvironmentValue{name: "KeepMe", value: []byte("keep-canary")},
	)
	notifier := &fakeUserEnvironmentNotifier{}
	controller := newUserEnvironmentController(store, notifier)

	result, err := controller.DeleteUserEnvironment(context.Background(), "deleteme")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || !result.RequiresNewSession || result.Operation != "delete" {
		t.Fatalf("unexpected delete result: %#v", result)
	}
	if _, exists := store.values["deleteme"]; exists {
		t.Fatal("selected variable remains after delete")
	}
	if entry, ok := store.values["keepme"]; !ok || string(entry.value) != "keep-canary" {
		t.Fatal("unselected variable did not survive delete")
	}

	result, err = controller.DeleteUserEnvironment(context.Background(), "absent")
	if err != nil || !result.Applied || result.RequiresNewSession {
		t.Fatalf("deleting an absent value should be an idempotent no-op: result=%#v err=%v", result, err)
	}
	if notifier.calls != 1 {
		t.Fatalf("absent delete should not broadcast, got %d notifications", notifier.calls)
	}
}

func TestUserEnvironmentInvalidInputIsRejectedBeforeStoreAccess(t *testing.T) {
	store := newFakeUserEnvironmentStore()
	controller := newUserEnvironmentController(store, &fakeUserEnvironmentNotifier{})
	tooLong := strings.Repeat("x", maxUserEnvironmentValueUTF16Units+1)
	tests := []struct {
		name  string
		write UserEnvironmentWrite
	}{
		{name: "empty name"},
		{name: "equals in name", write: UserEnvironmentWrite{Name: "BAD=NAME", Value: []byte("value")}},
		{name: "nul in name", write: UserEnvironmentWrite{Name: "BAD\x00NAME", Value: []byte("value")}},
		{name: "invalid utf8", write: UserEnvironmentWrite{Name: "NAME", Value: []byte{0xff}}},
		{name: "nul in value", write: UserEnvironmentWrite{Name: "NAME", Value: []byte("bad\x00value")}},
		{name: "overlong value", write: UserEnvironmentWrite{Name: "NAME", Value: []byte(tooLong)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := controller.SetUserEnvironment(context.Background(), test.write)
			if !errors.Is(err, errUserEnvironmentInvalid) {
				t.Fatalf("expected fixed validation error, got %v", err)
			}
		})
	}
	if store.readCalls != 0 || store.setCalls != 0 {
		t.Fatalf("invalid input reached the store: reads=%d writes=%d", store.readCalls, store.setCalls)
	}
	if _, err := controller.DeleteUserEnvironment(context.Background(), "BAD=NAME"); !errors.Is(err, errUserEnvironmentInvalid) {
		t.Fatalf("delete accepted invalid name: %v", err)
	}
}

func TestUserEnvironmentBackendErrorsDoNotExposeInputOrStoredValue(t *testing.T) {
	canary := "value-canary-that-must-not-escape"
	t.Run("read", func(t *testing.T) {
		store := newFakeUserEnvironmentStore()
		store.readErr = errors.New(canary)
		controller := newUserEnvironmentController(store, &fakeUserEnvironmentNotifier{})
		_, err := controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{Name: "PUBLIC_NAME", Value: []byte(canary)})
		if err == nil || strings.Contains(err.Error(), canary) {
			t.Fatalf("read failure was absent or exposed a value: %v", err)
		}
	})
	t.Run("write", func(t *testing.T) {
		store := newFakeUserEnvironmentStore()
		store.setErr = errors.New(canary)
		controller := newUserEnvironmentController(store, &fakeUserEnvironmentNotifier{})
		_, err := controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{Name: "PUBLIC_NAME", Value: []byte(canary)})
		if err == nil || strings.Contains(err.Error(), canary) {
			t.Fatalf("write failure was absent or exposed a value: %v", err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		store := newFakeUserEnvironmentStore(fakeUserEnvironmentValue{name: "PUBLIC_NAME", value: []byte(canary)})
		store.deleteErr = errors.New(canary)
		controller := newUserEnvironmentController(store, &fakeUserEnvironmentNotifier{})
		_, err := controller.DeleteUserEnvironment(context.Background(), "PUBLIC_NAME")
		if err == nil || strings.Contains(err.Error(), canary) {
			t.Fatalf("delete failure was absent or exposed a value: %v", err)
		}
	})
}

func TestUserEnvironmentNotificationFailureReportsAppliedPartialState(t *testing.T) {
	store := newFakeUserEnvironmentStore()
	notifier := &fakeUserEnvironmentNotifier{err: errors.New("synthetic notify failure")}
	controller := newUserEnvironmentController(store, notifier)

	result, err := controller.SetUserEnvironment(context.Background(), UserEnvironmentWrite{
		Name:  "SYNTHETIC_NAME",
		Value: []byte("synthetic-value"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || !result.RequiresNewSession || !strings.Contains(result.NextAction, "could not notify") {
		t.Fatalf("notification failure did not report the persisted partial state: %#v", result)
	}
	if value := store.values["synthetic_name"].value; string(value) != "synthetic-value" {
		t.Fatal("notification failure incorrectly implied the registry write was rolled back")
	}
}

func TestUserEnvironmentCanceledRequestDoesNotReachStore(t *testing.T) {
	store := newFakeUserEnvironmentStore()
	controller := newUserEnvironmentController(store, &fakeUserEnvironmentNotifier{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := controller.SetUserEnvironment(ctx, UserEnvironmentWrite{Name: "NAME", Value: []byte("value")}); err == nil {
		t.Fatal("canceled request was accepted")
	}
	if store.readCalls != 0 || store.setCalls != 0 {
		t.Fatalf("canceled request reached store: reads=%d writes=%d", store.readCalls, store.setCalls)
	}
}

func TestUserEnvironmentWriteValueIsNotSerialized(t *testing.T) {
	const canary = "json-canary-value"
	encoded, err := json.Marshal(UserEnvironmentWrite{Name: "NAME", Value: []byte(canary)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || strings.Contains(string(encoded), `"value"`) {
		t.Fatalf("write DTO serialized its raw value: %s", encoded)
	}
}
