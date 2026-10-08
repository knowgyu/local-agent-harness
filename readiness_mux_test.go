package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const readinessMuxTestHost = "127.0.0.1:43129"

func TestLocalUIMuxMountsPassiveReadinessBehindHostAndOriginGuards(t *testing.T) {
	configPath := writeReadinessConfig(t, config{Version: configVersion})
	registrations := &readinessRegistrationFake{}
	observer := &readinessObserverFake{observation: ResidentEndpointObservation{
		State:      ResidentEndpointResponding,
		ObservedAt: time.Date(2026, 10, 6, 2, 3, 4, 0, time.UTC),
	}}
	handler := newLocalUIHandlerWithResidentObserver(&app{
		configPath:         configPath,
		clientRegistration: registrations,
		host:               readinessMuxTestHost,
	}, observer)

	request := httptest.NewRequest(http.MethodGet, "http://"+readinessMuxTestHost+readinessStatusPath, nil)
	request.Host = readinessMuxTestHost
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("readiness GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(registrations.calls) != 0 || observer.calls != 1 {
		t.Fatalf("passive route calls: clients=%v observer=%d", registrations.calls, observer.calls)
	}

	cases := []struct {
		name       string
		host       string
		method     string
		origins    []string
		wantStatus int
	}{
		{name: "same origin", host: readinessMuxTestHost, method: http.MethodGet, origins: []string{"http://" + readinessMuxTestHost}, wantStatus: http.StatusOK},
		{name: "wrong host", host: "127.0.0.1:43130", method: http.MethodGet, wantStatus: http.StatusNotFound},
		{name: "foreign origin", host: readinessMuxTestHost, method: http.MethodGet, origins: []string{"http://attacker.invalid"}, wantStatus: http.StatusForbidden},
		{name: "duplicate origin", host: readinessMuxTestHost, method: http.MethodGet, origins: []string{"http://" + readinessMuxTestHost, "http://" + readinessMuxTestHost}, wantStatus: http.StatusForbidden},
		{name: "post rejected", host: readinessMuxTestHost, method: http.MethodPost, origins: []string{"http://" + readinessMuxTestHost}, wantStatus: http.StatusMethodNotAllowed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, "http://"+readinessMuxTestHost+readinessStatusPath, nil)
			request.Host = testCase.host
			for _, origin := range testCase.origins {
				request.Header.Add("Origin", origin)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status=%d, want %d: %s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
		})
	}
	if len(registrations.calls) != 0 || observer.calls != 2 {
		t.Fatalf("request dependency calls: clients=%v observer=%d; want no clients and two accepted GETs", registrations.calls, observer.calls)
	}
}
