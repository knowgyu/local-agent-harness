package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

type fakeLoginStartupValueStore struct {
	value     string
	exists    bool
	readErr   error
	writeErr  error
	deleteErr error
	writes    int
	deletes   int
}

func (s *fakeLoginStartupValueStore) Read() (string, bool, error) {
	return s.value, s.exists, s.readErr
}

func (s *fakeLoginStartupValueStore) Write(value string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.value, s.exists = value, true
	s.writes++
	return nil
}

func (s *fakeLoginStartupValueStore) Delete() error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.value, s.exists = "", false
	s.deletes++
	return nil
}

func TestManagedLoginStartupControllerIsIdempotentAndDeletesOnlyItsOwnCommand(t *testing.T) {
	store := &fakeLoginStartupValueStore{}
	controller, err := newManagedLoginStartupController(store, `"C:\Program Files\Local Agent Harness\agent.exe" --resident --background`)
	if err != nil {
		t.Fatal(err)
	}
	if got := controller.State(); got != loginStartupDisabled {
		t.Fatalf("initial state = %q, want disabled", got)
	}
	if err := controller.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(); err != nil {
		t.Fatalf("idempotent enable failed: %v", err)
	}
	if store.writes != 1 || !store.exists || controller.State() != loginStartupEnabled {
		t.Fatalf("after enable: writes=%d exists=%t state=%q", store.writes, store.exists, controller.State())
	}
	if err := controller.Disable(); err != nil {
		t.Fatal(err)
	}
	if err := controller.Disable(); err != nil {
		t.Fatalf("idempotent disable failed: %v", err)
	}
	if store.deletes != 1 || store.exists || controller.State() != loginStartupDisabled {
		t.Fatalf("after disable: deletes=%d exists=%t state=%q", store.deletes, store.exists, controller.State())
	}
}

func TestManagedLoginStartupControllerPreservesConflictingEntry(t *testing.T) {
	store := &fakeLoginStartupValueStore{value: `"C:\Other\agent.exe" --custom`, exists: true}
	controller, err := newManagedLoginStartupController(store, `"C:\Program Files\Local Agent Harness\agent.exe" --resident --background`)
	if err != nil {
		t.Fatal(err)
	}
	if got := controller.State(); got != loginStartupConflict {
		t.Fatalf("state = %q, want conflict", got)
	}
	if err := controller.Enable(); !errors.Is(err, errLoginStartupConflict) {
		t.Fatalf("conflicting enable error = %v, want reserved-name conflict", err)
	}
	if err := controller.Disable(); !errors.Is(err, errLoginStartupConflict) {
		t.Fatalf("conflicting disable error = %v, want reserved-name conflict", err)
	}
	if store.value != `"C:\Other\agent.exe" --custom` || store.writes != 0 || store.deletes != 0 {
		t.Fatal("controller changed a startup value it did not create")
	}
}

func TestManagedLoginStartupControllerMapsStoreFailuresToSafeState(t *testing.T) {
	readFailure := &fakeLoginStartupValueStore{readErr: errors.New("private registry detail")}
	controller, err := newManagedLoginStartupController(readFailure, "agent.exe --resident --background")
	if err != nil {
		t.Fatal(err)
	}
	if got := controller.State(); got != loginStartupUnavailable {
		t.Fatalf("state after registry read failure = %q, want unavailable", got)
	}
	if err := controller.Enable(); !errors.Is(err, errLoginStartupUnavailable) || strings.Contains(err.Error(), "private registry detail") {
		t.Fatalf("enable failure = %v, want fixed safe error", err)
	}

	writeFailure := &fakeLoginStartupValueStore{writeErr: errors.New("private registry detail")}
	controller, err = newManagedLoginStartupController(writeFailure, "agent.exe --resident --background")
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(); !errors.Is(err, errLoginStartupUnavailable) || strings.Contains(err.Error(), "private registry detail") {
		t.Fatalf("write failure = %v, want fixed safe error", err)
	}
}

func TestManagedLoginStartupControllerBoundsRegistryCommandLength(t *testing.T) {
	store := &fakeLoginStartupValueStore{}
	if _, err := newManagedLoginStartupController(store, strings.Repeat("a", windowsRunCommandMaxUTF16Units)); err != nil {
		t.Fatalf("command at the maximum length was rejected: %v", err)
	}
	if _, err := newManagedLoginStartupController(store, strings.Repeat("a", windowsRunCommandMaxUTF16Units+1)); !errors.Is(err, errLoginStartupCommand) {
		t.Fatalf("oversized command error = %v, want invalid-command error", err)
	}
	if _, err := newManagedLoginStartupController(store, "agent.exe\x00 --resident"); !errors.Is(err, errLoginStartupCommand) {
		t.Fatalf("NUL command error = %v, want invalid-command error", err)
	}
}

func TestAppLaunchModeKeepsDefaultUIAndUsesOnlyFixedResidentFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want appLaunchMode
		bad  bool
	}{
		{name: "default opens UI", args: nil, want: appLaunchUI},
		{name: "stdio client", args: []string{"--mcp"}, want: appLaunchMCP},
		{name: "visible resident", args: []string{"--resident"}, want: appLaunchResident},
		{name: "background resident", args: []string{"--resident", "--background"}, want: appLaunchResidentBackground},
		{name: "background flag alone rejected", args: []string{"--background"}, bad: true},
		{name: "caller cannot supply command", args: []string{"--resident", "--background", "--extra"}, bad: true},
		{name: "reordered flags rejected", args: []string{"--background", "--resident"}, bad: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseAppLaunchMode(test.args)
			if test.bad {
				if err == nil {
					t.Fatalf("mode for %q = %d, want rejected", test.args, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("mode for %q = %d, err=%v; want %d", test.args, got, err, test.want)
			}
		})
	}
}

func TestLoginStartupUIRequiresSameOriginCSRFAndUsesFixedActions(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &fakeLoginStartupValueStore{}
	startup, err := newManagedLoginStartupController(store, `"C:\Program Files\Local Agent Harness\agent.exe" --resident --background`)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, csrf: "login-startup-test-csrf", loginStartup: startup}
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	a.host = server.Listener.Addr().String()
	server.Config.Handler = newLocalUIHandler(a)
	server.Start()
	t.Cleanup(server.Close)

	client := *server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(page), `action="/set-login-startup"`) || !strings.Contains(string(page), a.csrf) {
		t.Fatalf("root page did not render the protected login startup form: status=%d", response.StatusCode)
	}
	post := func(action, csrf, origin string) *http.Response {
		t.Helper()
		form := url.Values{"action": {action}, "csrf": {csrf}}
		request, err := http.NewRequest(http.MethodPost, server.URL+"/set-login-startup", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	for _, test := range []struct {
		name   string
		csrf   string
		origin string
	}{
		{name: "missing CSRF", csrf: "", origin: server.URL},
		{name: "wrong Origin", csrf: a.csrf, origin: "http://attacker.example"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := post("enable", test.csrf, test.origin)
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden || store.writes != 0 {
				t.Fatalf("rejected POST status=%d writes=%d", response.StatusCode, store.writes)
			}
		})
	}
	response = post("enable", a.csrf, server.URL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || store.writes != 1 || startup.State() != loginStartupEnabled {
		t.Fatalf("enable POST status=%d writes=%d state=%q", response.StatusCode, store.writes, startup.State())
	}
	response = post("unexpected", a.csrf, server.URL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || store.writes != 1 || store.deletes != 0 {
		t.Fatalf("unknown action changed the registry value: status=%d writes=%d deletes=%d", response.StatusCode, store.writes, store.deletes)
	}
	response = post("disable", a.csrf, server.URL)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || store.deletes != 1 || startup.State() != loginStartupDisabled {
		t.Fatalf("disable POST status=%d deletes=%d state=%q", response.StatusCode, store.deletes, startup.State())
	}

	const existingValueCanary = `"C:\Private\Existing App\app.exe" --do-not-disclose`
	conflictStore := &fakeLoginStartupValueStore{value: existingValueCanary, exists: true}
	conflictStartup, err := newManagedLoginStartupController(conflictStore, `"C:\Program Files\Local Agent Harness\agent.exe" --resident --background`)
	if err != nil {
		t.Fatal(err)
	}
	conflictApp := &app{configPath: configPath, csrf: "login-startup-conflict-csrf", loginStartup: conflictStartup}
	conflictServer := httptest.NewUnstartedServer(http.NotFoundHandler())
	conflictApp.host = conflictServer.Listener.Addr().String()
	conflictServer.Config.Handler = newLocalUIHandler(conflictApp)
	conflictServer.Start()
	t.Cleanup(conflictServer.Close)
	conflictResponse, err := conflictServer.Client().Get(conflictServer.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	conflictPage, err := io.ReadAll(conflictResponse.Body)
	conflictResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if conflictResponse.StatusCode != http.StatusOK || !strings.Contains(string(conflictPage), "A different value uses the reserved startup name") || strings.Contains(string(conflictPage), existingValueCanary) || strings.Contains(string(conflictPage), `action="/set-login-startup"`) {
		t.Fatal("conflicting startup entry was exposed or offered for overwrite")
	}
	conflictForm := url.Values{"action": {"enable"}, "csrf": {conflictApp.csrf}}
	conflictRequest, err := http.NewRequest(http.MethodPost, conflictServer.URL+"/set-login-startup", strings.NewReader(conflictForm.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	conflictRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	conflictRequest.Header.Set("Origin", conflictServer.URL)
	conflictClient := *conflictServer.Client()
	conflictClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	conflictPOST, err := conflictClient.Do(conflictRequest)
	if err != nil {
		t.Fatal(err)
	}
	conflictPOST.Body.Close()
	if conflictPOST.StatusCode != http.StatusSeeOther || conflictStore.value != existingValueCanary || conflictStore.writes != 0 || conflictStore.deletes != 0 {
		t.Fatal("conflicting startup entry was modified by the UI action")
	}
}
