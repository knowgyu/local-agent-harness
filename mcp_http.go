package main

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpHTTPGatewayTokenBytes = 32
	mcpHTTPMaxRequestBytes   = 1 << 20
	mcpHTTPSessionTimeout    = 10 * time.Minute
)

// mcpHTTPTokenVerifier keeps the active gateway token replaceable so a future
// runtime can synchronize Credential Manager rotation and revocation with
// requests handled by the same server.
type mcpHTTPTokenVerifier struct {
	mu    sync.RWMutex
	token []byte
}

func newMCPHTTPTokenVerifier(token string) (*mcpHTTPTokenVerifier, error) {
	decoded, err := decodeMCPHTTPGatewayToken(token)
	if err != nil {
		return nil, err
	}
	return &mcpHTTPTokenVerifier{token: decoded}, nil
}

// replace serializes validation and replacement with revoke. A failed
// replacement leaves the current token active. Concurrent lifecycle calls are
// ordered by this lock; callers that need a particular external event order
// must serialize those calls. Clearing the prior byte slice is best-effort
// memory hygiene; it does not erase copies held elsewhere by Go's runtime.
func (v *mcpHTTPTokenVerifier) replace(token string) error {
	if v == nil {
		return errors.New("MCP HTTP token verifier is unavailable.")
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	decoded, err := decodeMCPHTTPGatewayToken(token)
	if err != nil {
		return err
	}
	clear(v.token)
	v.token = decoded
	return nil
}

func (v *mcpHTTPTokenVerifier) revoke() {
	if v == nil {
		return
	}
	v.mu.Lock()
	clear(v.token)
	v.token = nil
	v.mu.Unlock()
}

func (v *mcpHTTPTokenVerifier) allows(r *http.Request) bool {
	if v == nil || r == nil {
		return false
	}
	candidate, ok := decodeMCPHTTPBearer(r)
	if !ok {
		return false
	}
	defer clear(candidate)

	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.token) == mcpHTTPGatewayTokenBytes && subtle.ConstantTimeCompare(candidate, v.token) == 1
}

// newAuthenticatedMCPHTTPHandler exposes the supplied MCP server behind a
// loopback Host, same-origin, and shared bearer-token check. It does not create
// a listener; callers must bind one to a loopback IP before serving this handler.
func newAuthenticatedMCPHTTPHandler(server *mcp.Server, tokenVerifier *mcpHTTPTokenVerifier, expectedHost string) (http.Handler, error) {
	if server == nil {
		return nil, errors.New("MCP HTTP server is unavailable.")
	}
	if tokenVerifier == nil {
		return nil, errors.New("MCP HTTP token verifier is unavailable.")
	}
	if !validLoopbackMCPHTTPHost(expectedHost) {
		return nil, errors.New("MCP HTTP host must be a loopback IP address and port.")
	}

	transport := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			MaxRequestBodyBytes:          mcpHTTPMaxRequestBytes,
			SessionTimeout:               mcpHTTPSessionTimeout,
			PropagateRequestCancellation: true,
		},
	)
	expectedOrigin := "http://" + expectedHost

	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expectedHost {
			http.Error(w, "Request rejected.", http.StatusForbidden)
			return
		}
		if !mcpHTTPOriginAllowed(r, expectedOrigin) {
			http.Error(w, "Request origin rejected.", http.StatusForbidden)
			return
		}
		if !tokenVerifier.allows(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Authentication required.", http.StatusUnauthorized)
			return
		}

		transport.ServeHTTP(w, r)
	})
	crossOriginProtection := http.NewCrossOriginProtection().Handler(gateway)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setMCPHTTPResponseHeaders(w)
		crossOriginProtection.ServeHTTP(w, r)
	}), nil
}

func listenMCPHTTPLoopback(port int) (net.Listener, error) {
	if port < 0 || port > 65535 {
		return nil, errors.New("MCP HTTP port is invalid.")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, err
	}
	localAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || localAddress.IP == nil || !localAddress.IP.IsLoopback() {
		_ = listener.Close()
		return nil, errors.New("MCP HTTP listener did not bind to loopback.")
	}
	return listener, nil
}

func decodeMCPHTTPGatewayToken(token string) ([]byte, error) {
	if len(token) != hex.EncodedLen(mcpHTTPGatewayTokenBytes) {
		return nil, errors.New("MCP HTTP gateway token must contain 32 random bytes encoded as hexadecimal.")
	}
	decoded := make([]byte, mcpHTTPGatewayTokenBytes)
	tokenBytes := []byte(token)
	defer clear(tokenBytes)
	n, err := hex.Decode(decoded, tokenBytes)
	if err != nil || n != mcpHTTPGatewayTokenBytes {
		clear(decoded)
		return nil, errors.New("MCP HTTP gateway token must contain 32 random bytes encoded as hexadecimal.")
	}
	return decoded, nil
}

func validLoopbackMCPHTTPHost(host string) bool {
	ipText, portText, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	ip := net.ParseIP(ipText)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port > 0 && port <= 65535 && net.JoinHostPort(ip.String(), portText) == host
}

func mcpHTTPOriginAllowed(r *http.Request, expectedOrigin string) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && values[0] == expectedOrigin
}

func decodeMCPHTTPBearer(r *http.Request) ([]byte, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return nil, false
	}
	scheme, candidateText, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(candidateText) != hex.EncodedLen(mcpHTTPGatewayTokenBytes) {
		return nil, false
	}
	candidate := make([]byte, mcpHTTPGatewayTokenBytes)
	candidateBytes := []byte(candidateText)
	defer clear(candidateBytes)
	n, err := hex.Decode(candidate, candidateBytes)
	if err != nil || n != mcpHTTPGatewayTokenBytes {
		clear(candidate)
		return nil, false
	}
	return candidate, true
}

func setMCPHTTPResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
