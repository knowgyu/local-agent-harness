package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const residentHealthSubprocessFlag = "LOCAL_AGENT_HARNESS_TEST_RESIDENT_HEALTH_HELPER"

func TestResidentMCPHealthSubprocessHelper(t *testing.T) {
	if os.Getenv(residentHealthSubprocessFlag) != "1" {
		return
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "resident-health-test", Version: "1"}, nil)
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal("could not initialize isolated resident health fixture")
	}
	runtime, err := startMCPHTTPRuntime(server, verifier, 0)
	if err != nil {
		t.Fatal("could not start isolated resident health fixture")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
		defer cancel()
		if err := runtime.shutdown(ctx); err != nil {
			t.Error("could not stop isolated resident health fixture")
		}
	})

	port := runtime.listener.Addr().(*net.TCPAddr).Port
	if _, err := fmt.Fprintln(os.Stdout, port); err != nil {
		t.Fatal("could not publish isolated health fixture port")
	}
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestResidentEndpointObserverObservesSeparateRuntimeProcess(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestResidentMCPHealthSubprocessHelper$")
	command.Env = []string{residentHealthSubprocessFlag + "=1"}
	command.Dir = t.TempDir()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal("could not capture isolated health fixture port")
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal("could not control isolated health fixture lifetime")
	}
	if err := command.Start(); err != nil {
		t.Fatal("could not start isolated health fixture process")
	}
	commandDone := make(chan error, 1)
	go func() { commandDone <- command.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case err := <-commandDone:
			if err != nil {
				t.Error("isolated health fixture process did not exit cleanly")
			}
		case <-time.After(2 * mcpHTTPShutdownTimeout):
			_ = command.Process.Kill()
			<-commandDone
			t.Error("isolated health fixture process exceeded its shutdown bound")
		}
	})

	portLine, err := bufio.NewReader(io.LimitReader(stdout, 32)).ReadString('\n')
	if err != nil {
		t.Fatal("isolated health fixture did not provide a loopback port")
	}
	port, err := strconv.Atoi(strings.TrimSpace(portLine))
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("isolated health fixture provided an invalid loopback port")
	}
	observer, err := newResidentEndpointObserverAtPort(port)
	if err != nil {
		t.Fatal("could not initialize fixed-host observer for isolated port")
	}
	observation := observer.ObserveResidentEndpoint(context.Background())
	if observation.State != ResidentEndpointResponding {
		t.Fatalf("separate runtime observation state = %q, want %q", observation.State, ResidentEndpointResponding)
	}
	if observation.ObservedAt.IsZero() || observation.ObservedAt.Location() != time.UTC {
		t.Fatalf("observation timestamp = %v, want nonzero UTC completion time", observation.ObservedAt)
	}
}

func TestResidentMCPHealthRouteExactResponseAndRequestBoundaries(t *testing.T) {
	const expectedHost = "127.0.0.1:49321"
	handler := residentMCPHealthHandler(expectedHost)

	request := httptest.NewRequest(http.MethodGet, "http://"+expectedHost+residentMCPHealthPath, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("health response status = %d, want 200", response.Code)
	}
	if response.Header().Get("Content-Type") != residentMCPHealthContentType {
		t.Fatalf("health Content-Type = %q, want %q", response.Header().Get("Content-Type"), residentMCPHealthContentType)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
	if got := response.Body.String(); got != residentMCPHealthResponse {
		t.Fatalf("health response body = %q, want exact constant identity", got)
	}

	tests := []struct {
		name   string
		method string
		target string
		host   string
		origin []string
		body   string
		status int
	}{
		{name: "wrong host", method: http.MethodGet, target: "http://" + expectedHost + residentMCPHealthPath, host: "127.0.0.1:49322", status: http.StatusForbidden},
		{name: "foreign origin", method: http.MethodGet, target: "http://" + expectedHost + residentMCPHealthPath, origin: []string{"http://example.invalid"}, status: http.StatusForbidden},
		{name: "duplicate origins", method: http.MethodGet, target: "http://" + expectedHost + residentMCPHealthPath, origin: []string{"http://" + expectedHost, "http://" + expectedHost}, status: http.StatusForbidden},
		{name: "same origin", method: http.MethodGet, target: "http://" + expectedHost + residentMCPHealthPath, origin: []string{"http://" + expectedHost}, status: http.StatusOK},
		{name: "post rejected", method: http.MethodPost, target: "http://" + expectedHost + residentMCPHealthPath, status: http.StatusMethodNotAllowed},
		{name: "query rejected", method: http.MethodGet, target: "http://" + expectedHost + residentMCPHealthPath + "?x=1", status: http.StatusBadRequest},
		{name: "body rejected", method: http.MethodGet, target: "http://" + expectedHost + residentMCPHealthPath, body: "ignored", status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			if test.host != "" {
				request.Host = test.host
			}
			for _, origin := range test.origin {
				request.Header.Add("Origin", origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if response.Code != http.StatusOK && strings.Contains(response.Body.String(), "example.invalid") {
				t.Fatal("rejection response reflected untrusted request data")
			}
			if test.method == http.MethodPost && response.Header().Get("Allow") != http.MethodGet {
				t.Fatalf("Allow header = %q, want GET", response.Header().Get("Allow"))
			}
		})
	}
}

func TestResidentEndpointObserverRequiresExactBoundedIdentity(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		addType     bool
		want        ResidentEndpointObservationState
	}{
		{name: "exact identity", status: http.StatusOK, contentType: residentMCPHealthContentType, body: residentMCPHealthResponse, want: ResidentEndpointResponding},
		{name: "wrong status", status: http.StatusServiceUnavailable, contentType: residentMCPHealthContentType, body: residentMCPHealthResponse, want: ResidentEndpointUnknown},
		{name: "wrong identity", status: http.StatusOK, contentType: residentMCPHealthContentType, body: `{"service":"other"}`, want: ResidentEndpointUnknown},
		{name: "wrong content type", status: http.StatusOK, contentType: "text/plain", body: residentMCPHealthResponse, want: ResidentEndpointUnknown},
		{name: "content type parameters", status: http.StatusOK, contentType: "application/json; charset=utf-8", body: residentMCPHealthResponse, want: ResidentEndpointUnknown},
		{name: "oversize body", status: http.StatusOK, contentType: residentMCPHealthContentType, body: strings.Repeat("x", residentMCPObserverBodyLimit+1), want: ResidentEndpointUnknown},
		{name: "duplicate content type", status: http.StatusOK, contentType: residentMCPHealthContentType, body: residentMCPHealthResponse, addType: true, want: ResidentEndpointUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.addType {
					w.Header().Add("Content-Type", test.contentType)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			observer := observerForTestServer(t, server)
			observation := observer.ObserveResidentEndpoint(context.Background())
			if observation.State != test.want {
				t.Fatalf("observation state = %q, want %q", observation.State, test.want)
			}
			if observation.ObservedAt.IsZero() || observation.ObservedAt.Location() != time.UTC {
				t.Fatalf("observation timestamp = %v, want nonzero UTC completion time", observation.ObservedAt)
			}
		})
	}
}

func TestResidentEndpointObserverDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.Header().Set("Content-Type", residentMCPHealthContentType)
		_, _ = io.WriteString(w, residentMCPHealthResponse)
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, second.URL+residentMCPHealthPath, http.StatusFound)
	}))
	defer first.Close()

	observer := observerForTestServer(t, first)
	observation := observer.ObserveResidentEndpoint(context.Background())
	if observation.State != ResidentEndpointUnknown {
		t.Fatalf("redirect observation state = %q, want unknown", observation.State)
	}
	if redirected.Load() {
		t.Fatal("observer followed a redirect to another endpoint")
	}
}

func TestResidentEndpointObserverCancellationAndConnectionRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	observer := observerForTestServer(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if got := observer.ObserveResidentEndpoint(ctx).State; got != ResidentEndpointUnavailable {
		t.Fatalf("canceled observation state = %q, want unavailable", got)
	}
	server.Close()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not reserve a private loopback test port")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal("could not release the private loopback test port")
	}
	_, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if dialErr == nil {
		t.Fatal("private loopback test port unexpectedly accepted a connection")
	}
	refusedObserver, err := newResidentEndpointObserverAtPort(port)
	if err != nil {
		t.Fatal("could not initialize refusal test observer")
	}
	if got := refusedObserver.ObserveResidentEndpoint(context.Background()).State; got != ResidentEndpointNotReachable {
		t.Fatalf("connection-refused observation state = %q, want not_reachable", got)
	}
}

func TestResidentEndpointObserverRejectsInvalidTestPorts(t *testing.T) {
	for _, port := range []int{-1, 0, 65536} {
		if _, err := newResidentEndpointObserverAtPort(port); err == nil {
			t.Errorf("observer accepted invalid test port %d", port)
		}
	}
}

func observerForTestServer(t *testing.T, server *httptest.Server) *residentEndpointObserver {
	t.Helper()
	address, err := net.ResolveTCPAddr("tcp4", strings.TrimPrefix(server.URL, "http://"))
	if err != nil || address.IP == nil || !address.IP.IsLoopback() || address.Port < 1 {
		t.Fatal("test server did not bind to an assigned loopback address")
	}
	observer, err := newResidentEndpointObserverAtPort(address.Port)
	if err != nil {
		t.Fatal("could not initialize fixed-host observer for loopback test server")
	}
	return observer
}
