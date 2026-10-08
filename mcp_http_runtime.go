package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpHTTPShutdownTimeout = 5 * time.Second
	// Keep incomplete request headers bounded below graceful shutdown. net/http
	// otherwise waits up to five seconds before treating a StateNew connection
	// as idle, which can exhaust the shutdown window.
	mcpHTTPReadHeaderTimeout = 2 * time.Second
)

// mcpHTTPRuntime owns one loopback listener and revokes its verifier when the
// listener stops. It is a runtime building block; app startup and token
// provisioning remain separate concerns.
type mcpHTTPRuntime struct {
	listener net.Listener
	server   *http.Server
	verifier *mcpHTTPTokenVerifier
	done     chan struct{}
	serveErr error

	closeOnce sync.Once
	closeErr  error
}

func startMCPHTTPRuntime(server *mcp.Server, verifier *mcpHTTPTokenVerifier, port int) (*mcpHTTPRuntime, error) {
	listener, err := listenMCPHTTPLoopback(port)
	if err != nil {
		return nil, errors.New("MCP HTTP loopback listener could not be started.")
	}
	return startMCPHTTPRuntimeOnListener(server, verifier, listener)
}

func startMCPHTTPRuntimeOnListener(server *mcp.Server, verifier *mcpHTTPTokenVerifier, listener net.Listener) (*mcpHTTPRuntime, error) {
	if listener == nil {
		return nil, errors.New("MCP HTTP loopback listener is unavailable.")
	}
	if server == nil || verifier == nil {
		_ = listener.Close()
		return nil, errors.New("MCP HTTP runtime is unavailable.")
	}
	localAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || localAddress.IP == nil || !localAddress.IP.IsLoopback() || localAddress.Port == 0 {
		_ = listener.Close()
		return nil, errors.New("MCP HTTP listener must use an assigned loopback address.")
	}
	handler, err := newAuthenticatedMCPHTTPHandler(server, verifier, listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	healthHandler := residentMCPHealthHandler(listener.Addr().String())
	runtimeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setMCPHTTPResponseHeaders(w)
		if r.URL.Path == residentMCPHealthPath {
			healthHandler.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})

	runtime := &mcpHTTPRuntime{
		listener: listener,
		server: &http.Server{
			Handler:           runtimeHandler,
			ReadHeaderTimeout: mcpHTTPReadHeaderTimeout,
			ReadTimeout:       15 * time.Second,
			// Streamable HTTP uses long-lived event streams. A write deadline
			// would terminate an otherwise active MCP session.
			WriteTimeout:   0,
			IdleTimeout:    60 * time.Second,
			MaxHeaderBytes: 16 << 10,
		},
		verifier: verifier,
		done:     make(chan struct{}),
	}
	go func() {
		runtime.serveErr = runtime.server.Serve(runtime.listener)
		verifier.revoke()
		close(runtime.done)
	}()
	return runtime, nil
}

func (r *mcpHTTPRuntime) endpoint() string {
	if r == nil || r.listener == nil {
		return ""
	}
	return "http://" + r.listener.Addr().String() + "/mcp"
}

func (r *mcpHTTPRuntime) shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("MCP HTTP shutdown context is unavailable.")
	}
	r.closeOnce.Do(func() {
		r.verifier.revoke()
		shutdownCtx, cancel := context.WithTimeout(ctx, mcpHTTPShutdownTimeout)
		shutdownErr := r.server.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			_ = r.server.Close()
		}

		<-r.done
		if shutdownErr != nil {
			r.closeErr = errors.New("MCP HTTP server required a forced shutdown.")
			return
		}
		if r.serveErr != nil && !errors.Is(r.serveErr, http.ErrServerClosed) {
			r.closeErr = errors.New("MCP HTTP server stopped unexpectedly.")
		}
	})
	return r.closeErr
}

func (r *mcpHTTPRuntime) stopped() bool {
	if r == nil || r.done == nil {
		return true
	}
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}
