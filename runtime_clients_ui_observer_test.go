package main

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

type runtimeClientsUIResidentObserverFake struct {
	observation ResidentEndpointObservation
	calls       int
}

func (f *runtimeClientsUIResidentObserverFake) ObserveResidentEndpoint(context.Context) ResidentEndpointObservation {
	f.calls++
	return f.observation
}

func TestRuntimeClientsUIStatusIncludesInjectedResidentObservation(t *testing.T) {
	controller := newRuntimeClientsUIControllerFake()
	observedAt := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	observer := &runtimeClientsUIResidentObserverFake{
		observation: ResidentEndpointObservation{
			State:      ResidentEndpointResponding,
			ObservedAt: observedAt,
		},
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	handler := newRuntimeClientsUIHandlerWithResidentObserver(
		runtimeClientsUIRuntimeFake{status: MCPRuntimeStatus{State: mcpRuntimeStopped}},
		controller,
		"runtime-clients-csrf",
		runtimeClientsUITestHost,
		executable,
		clientRegistrationUIErrorCode,
		observer,
	)
	recorder := runtimeClientsUIServe(t, handler, http.MethodGet, runtimeClientsUIStatusPath, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeRuntimeClientsUIResponse(t, recorder)
	if observer.calls != 1 {
		t.Fatalf("resident observer calls=%d, want 1", observer.calls)
	}
	if response.Runtime == nil || response.Runtime.State != mcpRuntimeStopped {
		t.Fatalf("legacy lifecycle status was not preserved: %+v", response.Runtime)
	}
	if response.ResidentEndpoint == nil || response.ResidentEndpoint.State != ResidentEndpointResponding || !response.ResidentEndpoint.ObservedAt.Equal(observedAt) {
		t.Fatalf("resident endpoint observation missing or changed: %+v", response.ResidentEndpoint)
	}
}
