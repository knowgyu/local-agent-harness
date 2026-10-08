package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const userEnvironmentUICanary = "user-environment-ui-value-canary-3a91"

type userEnvironmentUIFakeController struct {
	setCalls     int
	deleteCalls  int
	lastWrite    UserEnvironmentWrite
	valueCopy    []byte
	valueRef     []byte
	lastName     string
	setResult    UserEnvironmentResult
	deleteResult UserEnvironmentResult
	setErr       error
	deleteErr    error
}

func (f *userEnvironmentUIFakeController) ListUserEnvironment(_ context.Context) ([]UserEnvironmentEntry, error) {
	return []UserEnvironmentEntry{}, nil
}

func (f *userEnvironmentUIFakeController) SetUserEnvironment(_ context.Context, write UserEnvironmentWrite) (UserEnvironmentResult, error) {
	f.setCalls++
	f.lastWrite = write
	f.lastName = write.Name
	f.valueRef = write.Value
	f.valueCopy = append([]byte(nil), write.Value...)
	if f.setErr != nil {
		return UserEnvironmentResult{}, f.setErr
	}
	if f.setResult.Name == "" {
		f.setResult = UserEnvironmentResult{Name: write.Name, Operation: "set", Applied: true, RequiresNewSession: true}
	}
	return f.setResult, nil
}

func (f *userEnvironmentUIFakeController) DeleteUserEnvironment(_ context.Context, name string) (UserEnvironmentResult, error) {
	f.deleteCalls++
	f.lastName = name
	if f.deleteErr != nil {
		return UserEnvironmentResult{}, f.deleteErr
	}
	if f.deleteResult.Name == "" {
		f.deleteResult = UserEnvironmentResult{Name: name, Operation: "delete", Applied: true, RequiresNewSession: true}
	}
	return f.deleteResult, nil
}

func TestUserEnvironmentUISetKeepsValueAndUntrustedNextActionOutOfResponse(t *testing.T) {
	host := "127.0.0.1:49501"
	controller := &userEnvironmentUIFakeController{setResult: UserEnvironmentResult{
		Name: "BUILD_TOKEN", Operation: "set", Applied: true, RequiresNewSession: true, NextAction: userEnvironmentUICanary,
	}}
	handler := newUserEnvironmentUIHandler(host, "csrf-env", controller)
	request := userEnvironmentUIRequest(t, host, uiRouteSaveUserEnvironment, url.Values{
		"csrf": {"csrf-env"}, "name": {"BUILD_TOKEN"}, "value": {userEnvironmentUICanary},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || controller.setCalls != 1 || string(controller.valueCopy) != userEnvironmentUICanary {
		t.Fatalf("set result status=%d calls=%d value=%q body=%s", response.Code, controller.setCalls, controller.valueCopy, response.Body.String())
	}
	if controller.lastWrite.Name != "BUILD_TOKEN" || len(controller.valueRef) == 0 || !allBytesZero(controller.valueRef) {
		t.Fatalf("value buffer was not cleared or name changed: %+v value=%v", controller.lastWrite, controller.valueRef)
	}
	body := response.Body.String()
	var got userEnvironmentUISuccess
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Result.Name != "BUILD_TOKEN" || got.Result.Operation != "set" || !got.Result.Applied || !got.Result.RequiresNewSession || got.Result.NextAction != "new_session" {
		t.Fatalf("unexpected safe result: %+v", got)
	}
	if strings.Contains(body, userEnvironmentUICanary) || strings.Contains(body, "value") {
		t.Fatalf("response exposed an environment value or controller next-action: %s", body)
	}
}

func TestUserEnvironmentUIDeleteReturnsOnlyFixedResult(t *testing.T) {
	host := "127.0.0.1:49502"
	controller := &userEnvironmentUIFakeController{deleteResult: UserEnvironmentResult{
		Name: "SDK_TOKEN", Operation: "delete", Applied: true, RequiresNewSession: true, NextAction: "arbitrary private guidance",
	}}
	handler := newUserEnvironmentUIHandler(host, "csrf-env", controller)
	request := userEnvironmentUIRequest(t, host, uiRouteDeleteUserEnvironment, url.Values{"csrf": {"csrf-env"}, "name": {"SDK_TOKEN"}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || controller.deleteCalls != 1 || controller.lastName != "SDK_TOKEN" {
		t.Fatalf("delete result status=%d calls=%d name=%q body=%s", response.Code, controller.deleteCalls, controller.lastName, body)
	}
	if strings.Contains(body, "arbitrary private guidance") || strings.Contains(body, "value") {
		t.Fatalf("response included untrusted controller text: %s", body)
	}
	if !strings.Contains(body, `"operation":"delete"`) || !strings.Contains(body, `"next_action":"new_session"`) {
		t.Fatalf("expected fixed delete result: %s", body)
	}
}

func TestUserEnvironmentUIHidesBackendErrorsAndRejectsUnmatchedResults(t *testing.T) {
	host := "127.0.0.1:49503"
	controller := &userEnvironmentUIFakeController{setErr: errors.New(userEnvironmentUICanary)}
	handler := newUserEnvironmentUIHandler(host, "csrf-env", controller)
	request := userEnvironmentUIRequest(t, host, uiRouteSaveUserEnvironment, url.Values{
		"csrf": {"csrf-env"}, "name": {"BUILD_TOKEN"}, "value": {userEnvironmentUICanary},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), userEnvironmentUICanary) {
		t.Fatalf("backend error escaped: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"error":"operation_failed"`) {
		t.Fatalf("unexpected fixed backend error response: %s", response.Body.String())
	}

	controller = &userEnvironmentUIFakeController{setResult: UserEnvironmentResult{Name: userEnvironmentUICanary, Operation: "set", Applied: true}}
	handler = newUserEnvironmentUIHandler(host, "csrf-env", controller)
	request = userEnvironmentUIRequest(t, host, uiRouteSaveUserEnvironment, url.Values{
		"csrf": {"csrf-env"}, "name": {"BUILD_TOKEN"}, "value": {"safe-value"},
	})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), userEnvironmentUICanary) {
		t.Fatalf("mismatched controller result was not hidden: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUserEnvironmentUIRejectsInvalidNamesOriginCSRFAndEmptyValues(t *testing.T) {
	tests := []struct {
		name   string
		values url.Values
		origin string
		status int
	}{
		{name: "equals in name", values: url.Values{"csrf": {"csrf-env"}, "name": {"BUILD=TOKEN"}, "value": {"x"}}, origin: "http://127.0.0.1:49504", status: http.StatusBadRequest},
		{name: "null origin", values: url.Values{"csrf": {"csrf-env"}, "name": {"BUILD_TOKEN"}, "value": {"x"}}, origin: "null", status: http.StatusForbidden},
		{name: "wrong csrf", values: url.Values{"csrf": {"wrong"}, "name": {"BUILD_TOKEN"}, "value": {"x"}}, origin: "http://127.0.0.1:49504", status: http.StatusForbidden},
	}
	host := "127.0.0.1:49504"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &userEnvironmentUIFakeController{}
			handler := newUserEnvironmentUIHandler(host, "csrf-env", controller)
			request := userEnvironmentUIRequest(t, host, uiRouteSaveUserEnvironment, test.values)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || controller.setCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, controller.setCalls, response.Body.String())
			}
		})
	}
	controller := &userEnvironmentUIFakeController{}
	handler := newUserEnvironmentUIHandler(host, "csrf-env", controller)
	request := userEnvironmentUIRequest(t, host, uiRouteSaveUserEnvironment, url.Values{"csrf": {"csrf-env"}, "name": {"A\x00B"}, "value": {"x"}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || controller.setCalls != 0 {
		t.Fatalf("NUL name status=%d calls=%d", response.Code, controller.setCalls)
	}
}

func TestUserEnvironmentUIAcceptsEmptyValueAsConfigured(t *testing.T) {
	const host = "127.0.0.1:49506"
	controller := &userEnvironmentUIFakeController{}
	handler := newUserEnvironmentUIHandler(host, "csrf-env", controller)
	request := userEnvironmentUIRequest(t, host, uiRouteSaveUserEnvironment, url.Values{
		"csrf": {"csrf-env"}, "name": {"EMPTY_VALUE"}, "value": {""},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || controller.setCalls != 1 || controller.lastName != "EMPTY_VALUE" || len(controller.valueCopy) != 0 {
		t.Fatalf("empty value was not accepted: status=%d calls=%d name=%q value=%q body=%s", response.Code, controller.setCalls, controller.lastName, controller.valueCopy, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"operation":"set"`) || strings.Contains(response.Body.String(), `"value"`) {
		t.Fatalf("empty-value success response was malformed or exposed a value: %s", response.Body.String())
	}
}

func userEnvironmentUIRequest(t *testing.T, host, path string, values url.Values) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://"+host+path, strings.NewReader(values.Encode()))
	request.Host = host
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	request.Header.Set("Origin", "http://"+host)
	return request
}

func replaceUserEnvironmentUIRequestBody(request *http.Request, body string) {
	request.Body = io.NopCloser(strings.NewReader(body))
	request.ContentLength = int64(len(body))
}
