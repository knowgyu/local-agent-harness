package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	residentMCPHealthPath        = "/.well-known/local-agent-harness/resident-mcp-health"
	residentMCPHealthContentType = "application/json"
	residentMCPHealthResponse    = `{"service":"local-agent-harness","protocol":"resident-mcp-health-v1","status":"responding"}`
	residentMCPObserverTimeout   = time.Second
	residentMCPObserverBodyLimit = 256
)

// ResidentEndpointObservationState describes what the fixed local resident
// endpoint probe observed. A response does not prove that an MCP tool call
// succeeded or that the responding process belongs to this application.
type ResidentEndpointObservationState string

const (
	ResidentEndpointResponding   ResidentEndpointObservationState = "responding"
	ResidentEndpointNotReachable ResidentEndpointObservationState = "not_reachable"
	ResidentEndpointUnknown      ResidentEndpointObservationState = "unknown"
	ResidentEndpointUnavailable  ResidentEndpointObservationState = "unavailable"
)

// ResidentEndpointObservation is a point-in-time result from probing the
// fixed loopback resident MCP health endpoint.
type ResidentEndpointObservation struct {
	State      ResidentEndpointObservationState `json:"state"`
	ObservedAt time.Time                        `json:"observed_at"`
}

// ResidentEndpointObservationProvider performs a read-only resident endpoint
// observation. It has no lifecycle or credential operations.
type ResidentEndpointObservationProvider interface {
	ObserveResidentEndpoint(context.Context) ResidentEndpointObservation
}

type residentEndpointObserver struct {
	port   int
	client *http.Client
}

func newResidentEndpointObserver() ResidentEndpointObservationProvider {
	observer, err := newResidentEndpointObserverAtPort(mcpHTTPDefaultPort)
	if err != nil {
		// The production port is a compile-time valid TCP port. Keep the public
		// constructor infallible so callers cannot substitute a different host.
		return unavailableResidentEndpointObserver{}
	}
	return observer
}

type unavailableResidentEndpointObserver struct{}

func (unavailableResidentEndpointObserver) ObserveResidentEndpoint(context.Context) ResidentEndpointObservation {
	return ResidentEndpointObservation{
		State:      ResidentEndpointUnavailable,
		ObservedAt: time.Now().UTC(),
	}
}

func newResidentEndpointObserverAtPort(port int) (*residentEndpointObserver, error) {
	if port < 1 || port > 65535 {
		return nil, errors.New("resident endpoint observer port is invalid")
	}
	transport := &http.Transport{
		Proxy:              nil,
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext: (&net.Dialer{
			Timeout: residentMCPObserverTimeout,
		}).DialContext,
	}
	return &residentEndpointObserver{
		port: port,
		client: &http.Client{
			Transport: transport,
			Timeout:   residentMCPObserverTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (o *residentEndpointObserver) ObserveResidentEndpoint(ctx context.Context) (observation ResidentEndpointObservation) {
	observation.State = ResidentEndpointUnavailable
	defer func() {
		observation.ObservedAt = time.Now().UTC()
	}()
	if o == nil || o.client == nil || o.port < 1 || o.port > 65535 || ctx == nil {
		return observation
	}

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(o.port))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+residentMCPHealthPath, nil)
	if err != nil {
		return observation
	}
	request.Header.Set("Accept", residentMCPHealthContentType)
	request.Header.Set("Accept-Encoding", "identity")

	response, err := o.client.Do(request)
	if err != nil {
		if ctx.Err() == nil && isResidentEndpointConnectionRefused(err) {
			observation.State = ResidentEndpointNotReachable
		}
		return observation
	}
	defer response.Body.Close()

	if response.ContentLength > residentMCPObserverBodyLimit {
		observation.State = ResidentEndpointUnknown
		return observation
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, residentMCPObserverBodyLimit+1))
	if err != nil {
		return observation
	}
	contentTypes := response.Header.Values("Content-Type")
	contentEncodings := response.Header.Values("Content-Encoding")
	if response.StatusCode != http.StatusOK || len(contentTypes) != 1 || contentTypes[0] != residentMCPHealthContentType ||
		len(contentEncodings) != 0 || len(body) > residentMCPObserverBodyLimit || string(body) != residentMCPHealthResponse {
		observation.State = ResidentEndpointUnknown
		return observation
	}

	observation.State = ResidentEndpointResponding
	return observation
}

func residentMCPHealthHandler(expectedHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expectedHost || !mcpHTTPOriginAllowed(r, "http://"+expectedHost) {
			http.Error(w, "Request rejected.", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != residentMCPHealthPath || r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "Request rejected.", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", residentMCPHealthContentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, residentMCPHealthResponse)
	})
}
