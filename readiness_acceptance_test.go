package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const readinessAcceptanceMuxHost = "127.0.0.1:43174"

func TestReadinessAcceptanceMuxReadOnlyPageAndExactInspectionOptIn(t *testing.T) {
	configPath := writeReadinessConfig(t, config{Version: configVersion})
	registrations := &readinessRegistrationFake{}
	observedAt := time.Date(2026, 10, 6, 4, 5, 6, 0, time.UTC)
	observer := &readinessObserverFake{observation: ResidentEndpointObservation{
		State:      ResidentEndpointUnknown,
		ObservedAt: observedAt,
	}}
	handler := newLocalUIHandlerWithResidentObserver(&app{
		configPath:         configPath,
		clientRegistration: registrations,
		host:               readinessAcceptanceMuxHost,
	}, observer)

	pageRequest := httptest.NewRequest(http.MethodGet, "http://"+readinessAcceptanceMuxHost+"/readiness", nil)
	pageRequest.Host = readinessAcceptanceMuxHost
	pageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("readiness page status = %d, want %d", pageResponse.Code, http.StatusOK)
	}
	if got := pageResponse.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("readiness page Content-Type = %q", got)
	}
	csp := pageResponse.Header().Get("Content-Security-Policy")
	nonceMatch := regexp.MustCompile(`<script nonce="([^"]+)"`).FindStringSubmatch(pageResponse.Body.String())
	scriptSourcePolicy := ""
	for _, directive := range strings.Split(csp, ";") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(directive, "script-src ") {
			scriptSourcePolicy = directive
			break
		}
	}
	if len(nonceMatch) != 2 || !strings.Contains(scriptSourcePolicy, "'nonce-"+nonceMatch[1]+"'") ||
		strings.Contains(scriptSourcePolicy, "'unsafe-inline'") {
		t.Fatal("mux response did not preserve the readiness page's script nonce policy")
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Fatal("readiness page CSP does not restrict fetches to the same origin")
	}
	if observer.calls != 0 || len(registrations.calls) != 0 {
		t.Fatalf("loading the page triggered status work: observer=%d clients=%v", observer.calls, registrations.calls)
	}

	statusRequest := httptest.NewRequest(
		http.MethodGet,
		"http://"+readinessAcceptanceMuxHost+readinessStatusPath+"?inspect_clients=1",
		nil,
	)
	statusRequest.Host = readinessAcceptanceMuxHost
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("exact readiness status query = %d, want %d", statusResponse.Code, http.StatusOK)
	}
	var status readinessStatusDTO
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode readiness status: %v", err)
	}
	wantClients := []string{mcpClientIDCodex, mcpClientIDClaude, mcpClientIDGemini}
	if strings.Join(registrations.calls, ",") != strings.Join(wantClients, ",") {
		t.Fatalf("mux inspected clients %v, want fixed supported clients", registrations.calls)
	}
	if observer.calls != 1 || status.ResidentEndpoint.State != string(ResidentEndpointUnknown) ||
		status.ResidentEndpoint.ObservedAt == nil || !status.ResidentEndpoint.ObservedAt.Equal(observedAt) {
		t.Fatal("status route did not use the injected observer result")
	}

	for _, query := range []string{
		"inspect_clients=1&client=caller-controlled&client=unsupported",
		"inspect_clients=claude&client=codex",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://"+readinessAcceptanceMuxHost+readinessStatusPath+"?"+query, nil)
		request.Host = readinessAcceptanceMuxHost
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("non-exact readiness status query = %d, want %d", recorder.Code, http.StatusOK)
		}
		if strings.Join(registrations.calls, ",") != strings.Join(wantClients, ",") {
			t.Fatalf("query %q changed the fixed inspection set: %v", query, registrations.calls)
		}
	}
	if observer.calls != 3 {
		t.Fatalf("status observer calls = %d, want one call per status GET", observer.calls)
	}
}
