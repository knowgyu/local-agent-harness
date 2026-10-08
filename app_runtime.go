package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpHTTPDefaultPort = 49321
	mcpHTTPPath        = "/mcp"

	mcpRuntimeStopped = "stopped"
	mcpRuntimeRunning = "running"
	mcpRuntimeError   = "error"
)

// residentMCPRuntimeController serializes lifecycle changes in one process.
// The fixed loopback listener is also the cross-process single-owner gate: a
// second app process cannot bind the same endpoint while its owner is alive.
type residentMCPRuntimeController struct {
	mu            sync.Mutex
	tokens        *mcpGatewayTokenManager
	serverFactory func() *mcp.Server
	port          int
	runtime       *mcpHTTPRuntime
	state         string
	changedAt     time.Time
	failureCode   string
}

func newResidentMCPRuntimeController(
	provider gatewayTokenCredentialProvider,
	serverFactory func() *mcp.Server,
	port int,
) *residentMCPRuntimeController {
	return &residentMCPRuntimeController{
		tokens:        newMCPGatewayTokenManager(provider),
		serverFactory: serverFactory,
		port:          port,
		state:         mcpRuntimeStopped,
		changedAt:     time.Now().UTC(),
	}
}

func (r *residentMCPRuntimeController) Status(ctx context.Context) (MCPRuntimeStatus, error) {
	if ctx == nil {
		return MCPRuntimeStatus{}, errors.New("MCP runtime status context is unavailable.")
	}
	if err := ctx.Err(); err != nil {
		return MCPRuntimeStatus{}, err
	}
	if r == nil {
		return MCPRuntimeStatus{}, errors.New("MCP runtime controller is unavailable.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observeUnexpectedStopLocked()
	return r.statusLocked(), nil
}

func (r *residentMCPRuntimeController) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("MCP runtime start context is unavailable.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return errors.New("MCP runtime controller is unavailable.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked(ctx)
}

func (r *residentMCPRuntimeController) startLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.runtime != nil && !r.runtime.stopped() {
		return nil
	}
	if r.runtime != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
		cleanupErr := r.runtime.shutdown(cleanupCtx)
		cancel()
		if cleanupErr != nil {
			r.state = mcpRuntimeError
			r.failureCode = "runtime_cleanup_failed"
			r.changedAt = time.Now().UTC()
			return errors.New("MCP HTTP runtime could not be restarted safely.")
		}
		r.runtime = nil
	}
	if r.serverFactory == nil || r.tokens == nil {
		r.setFailureLocked("runtime_unavailable")
		return errors.New("MCP runtime is unavailable.")
	}
	server := r.serverFactory()
	if server == nil {
		r.setFailureLocked("runtime_unavailable")
		return errors.New("MCP runtime is unavailable.")
	}
	listener, err := listenMCPHTTPLoopback(r.port)
	if err != nil {
		r.setFailureLocked("endpoint_unavailable")
		return errors.New("MCP HTTP loopback endpoint could not be started.")
	}
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		return err
	}

	token, err := r.tokens.loadOrCreate()
	if err != nil {
		_ = listener.Close()
		r.setFailureLocked("gateway_token_unavailable")
		return errors.New("MCP HTTP gateway token is unavailable.")
	}
	if err := ctx.Err(); err != nil {
		clear(token)
		_ = listener.Close()
		return err
	}
	encodedToken, err := encodeMCPGatewayToken(token)
	clear(token)
	if err != nil {
		_ = listener.Close()
		r.setFailureLocked("gateway_token_invalid")
		return errors.New("MCP HTTP gateway token is invalid.")
	}
	verifier, err := newMCPHTTPTokenVerifier(encodedToken)
	encodedToken = ""
	if err != nil {
		_ = listener.Close()
		r.setFailureLocked("gateway_token_invalid")
		return errors.New("MCP HTTP gateway token is invalid.")
	}
	runtime, err := startMCPHTTPRuntimeOnListener(server, verifier, listener)
	if err != nil {
		verifier.revoke()
		r.setFailureLocked("runtime_start_failed")
		return errors.New("MCP HTTP runtime could not be started.")
	}
	r.runtime = runtime
	r.state = mcpRuntimeRunning
	r.failureCode = ""
	r.changedAt = time.Now().UTC()
	return nil
}

func (r *residentMCPRuntimeController) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("MCP runtime stop context is unavailable.")
	}
	if r == nil {
		return errors.New("MCP runtime controller is unavailable.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopLocked(ctx)
}

func (r *residentMCPRuntimeController) Restart(ctx context.Context) error {
	if ctx == nil {
		return errors.New("MCP runtime restart context is unavailable.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return errors.New("MCP runtime controller is unavailable.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.stopLocked(ctx); err != nil {
		return err
	}
	return r.startLocked(ctx)
}

func (r *residentMCPRuntimeController) RotateGatewayToken(ctx context.Context) error {
	if ctx == nil {
		return errors.New("MCP gateway token rotation context is unavailable.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return errors.New("MCP runtime controller is unavailable.")
	}
	if r.tokens == nil {
		return errors.New("MCP HTTP gateway token storage is unavailable.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, next, err := r.tokens.rotate()
	if err != nil {
		if errors.Is(err, errMCPGatewayTokenInvalid) {
			return errors.New("MCP HTTP gateway token is invalid.")
		}
		return errors.New("MCP HTTP gateway token could not be rotated.")
	}
	defer clear(previous)
	defer clear(next)
	if r.runtime != nil && !r.runtime.stopped() {
		encoded, encodeErr := encodeMCPGatewayToken(next)
		if encodeErr != nil {
			_ = r.restorePreviousGatewayToken(previous)
			return errors.New("MCP HTTP gateway token could not be rotated.")
		}
		replaceErr := r.runtime.verifier.replace(encoded)
		encoded = ""
		if replaceErr != nil {
			if restoreErr := r.restorePreviousGatewayToken(previous); restoreErr != nil {
				_ = r.stopLocked(ctx)
				r.setFailureLocked("gateway_token_rotation_failed")
				return errors.New("MCP HTTP gateway token rotation failed; runtime was stopped.")
			}
			return errors.New("MCP HTTP gateway token could not be rotated.")
		}
	}
	r.changedAt = time.Now().UTC()
	return nil
}

func (r *residentMCPRuntimeController) RevokeGatewayToken(ctx context.Context) error {
	if ctx == nil {
		return errors.New("MCP gateway token revocation context is unavailable.")
	}
	if r == nil {
		return errors.New("MCP runtime controller is unavailable.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.tokens == nil {
		return errors.New("MCP HTTP gateway token storage is unavailable.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.stopLocked(ctx); err != nil {
		// shutdown revokes the in-memory verifier even when draining fails.
		// Continue deleting the persisted credential, but preserve the shutdown
		// failure status if deletion succeeds.
		if tokenErr := r.tokens.revoke(); tokenErr != nil {
			r.setFailureLocked("gateway_token_revoke_failed")
			return errors.New("MCP HTTP gateway token could not be revoked.")
		}
		return err
	}
	if err := r.tokens.revoke(); err != nil {
		r.setFailureLocked("gateway_token_revoke_failed")
		return errors.New("MCP HTTP gateway token could not be revoked.")
	}
	r.state = mcpRuntimeStopped
	r.failureCode = ""
	r.changedAt = time.Now().UTC()
	return nil
}

func (r *residentMCPRuntimeController) stopLocked(ctx context.Context) error {
	if r.runtime == nil {
		r.state = mcpRuntimeStopped
		r.failureCode = ""
		r.changedAt = time.Now().UTC()
		return nil
	}
	err := r.runtime.shutdown(ctx)
	r.runtime = nil
	r.changedAt = time.Now().UTC()
	if err != nil {
		r.state = mcpRuntimeError
		r.failureCode = "shutdown_failed"
		return errors.New("MCP HTTP runtime did not shut down cleanly.")
	}
	r.state = mcpRuntimeStopped
	r.failureCode = ""
	return nil
}

func (r *residentMCPRuntimeController) observeUnexpectedStopLocked() {
	if r.runtime == nil || !r.runtime.stopped() {
		return
	}
	if r.runtime.serveErr != nil && !errors.Is(r.runtime.serveErr, http.ErrServerClosed) {
		r.state = mcpRuntimeError
		r.failureCode = "runtime_stopped"
		r.changedAt = time.Now().UTC()
	}
}

func (r *residentMCPRuntimeController) statusLocked() MCPRuntimeStatus {
	endpoint := ""
	if r.port > 0 && r.port <= 65535 {
		endpoint = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port)) + mcpHTTPPath
	}
	if r.runtime != nil && r.runtime.listener != nil {
		endpoint = r.runtime.endpoint()
	}
	return MCPRuntimeStatus{
		State:       r.state,
		MCPEndpoint: endpoint,
		ChangedAt:   r.changedAt,
		FailureCode: r.failureCode,
	}
}

func (r *residentMCPRuntimeController) setFailureLocked(code string) {
	r.runtime = nil
	r.state = mcpRuntimeError
	r.failureCode = code
	r.changedAt = time.Now().UTC()
}

func (r *residentMCPRuntimeController) restorePreviousGatewayToken(previous []byte) error {
	if len(previous) == mcpGatewayTokenSize {
		return r.tokens.restore(previous)
	}
	return r.tokens.revoke()
}

func runResidentMCP() error {
	path, err := configFilePath()
	if err != nil {
		return errors.New("Could not locate the local settings file.")
	}
	if err := migrateConfig(path); err != nil {
		return errors.New("Could not migrate local settings. Previous settings remain in place.")
	}
	if !credentialStoreAvailable() {
		return errors.New("The resident MCP runtime requires Windows Credential Manager.")
	}
	a := &app{configPath: path, secrets: systemSecretStore{}, client: newGitHubClient()}
	if err := a.initializeRuntimeControllers(); err != nil {
		return errors.New("Could not initialize local registration and SSH settings.")
	}
	runtime := newResidentMCPRuntimeController(
		newSystemMCPGatewayTokenCredentialProvider(),
		a.mcpServer,
		mcpHTTPDefaultPort,
	)
	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignal()
	if err := runtime.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
	defer cancel()
	return runtime.Stop(shutdownCtx)
}

var _ MCPRuntimeController = (*residentMCPRuntimeController)(nil)

// runResidentMCPEntrypoint is the fixed resident command entry point. A
// build-tagged native QA binary may replace it with an isolated runtime seam;
// normal builds always use runResidentMCP.
var runResidentMCPEntrypoint = runResidentMCP
