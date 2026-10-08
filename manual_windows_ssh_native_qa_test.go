//go:build windows

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/windows"
)

const q01NativeAcceptanceEnv = "LAH_MANUAL_WINDOWS_SSH_NATIVE_QA"

// TestManualWindowsSSHNativeAcceptance is opt-in because it starts the
// installed Windows OpenSSH client. It connects only to a private loopback
// protocol server that records fixed SSH exec requests and never runs them.
func TestManualWindowsSSHNativeAcceptance(t *testing.T) {
	if os.Getenv(q01NativeAcceptanceEnv) != "1" {
		t.Skip("set LAH_MANUAL_WINDOWS_SSH_NATIVE_QA=1 on Windows to run the native SSH acceptance")
	}
	if runtimeGOOS() != "windows" {
		t.Skip("native OpenSSH acceptance requires Windows")
	}
	sshPath, err := resolveOpenSSHExecutable()
	if err != nil {
		t.Fatal("installed Windows OpenSSH client was not found")
	}
	if !strings.EqualFold(filepath.Base(sshPath), "ssh.exe") {
		t.Fatal("resolved client was not ssh.exe")
	}

	privateDir := filepath.Join(t.TempDir(), "Q01 private SSH materials")
	if err := os.Mkdir(privateDir, 0700); err != nil {
		t.Fatal("could not create the owned private-material directory")
	}
	q01NativeRestrictDirectory(t, privateDir)
	clientSigner, clientKeyPath := q01NativeWriteClientKey(t, privateDir)
	clientPublicKey := clientSigner.PublicKey()

	t.Run("fixed read-only and approved operation use the real client", func(t *testing.T) {
		server := q01NativeStartServer(t, q01NativeServerOptions{authorizedKey: clientPublicKey})
		knownHostsPath := filepath.Join(privateDir, "read-only known_hosts")
		q01NativeWriteKnownHosts(t, knownHostsPath, server.address(), server.hostPublicKey)
		configPath := q01NativeWriteSSHConfig(t, privateDir, "read-only ssh config", knownHostsPath, server.port())
		runner := q01NativeNewRunner(sshPath, configPath, clientKeyPath)

		readOperation := q01NativeReadOperation()
		preapprovedOperation := q01NativePreapprovedOperation()
		perCallOperation := q01NativePerCallOperation()
		target := q01NativeTarget(readOperation, preapprovedOperation, perCallOperation)
		approver := &q01NativeApprover{approved: true}
		controller := q01NativeController(target, runner, approver)

		readResult, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
			TargetID: target.ID, OperationID: readOperation.ID,
		})
		if err != nil || readResult.Status != "succeeded" || readResult.RemoteOutcome != SSHRemoteOutcomeConfirmed || !strings.Contains(readResult.Output, "stdout:\nQ01_NATIVE_SSH_OK") {
			t.Fatalf("native read-only roundtrip failed safely: status=%s remote=%s output_bytes=%d requests=%d", readResult.Status, readResult.RemoteOutcome, len(readResult.Output), server.execCount.Load())
		}
		if got := server.takeCommand(t); got != "q01-native-read status" {
			t.Fatal("native client did not send the fixed read-only command")
		}
		if approver.calls.Load() != 0 {
			t.Fatal("read-only operation unexpectedly requested approval")
		}

		wrongScope, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
			TargetID: target.ID, OperationID: preapprovedOperation.ID,
			Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"q01-other-scope"`)},
		})
		if err != nil || wrongScope.Status != "rejected" || runner.starts.Load() != 1 || server.execCount.Load() != 1 {
			t.Fatal("out-of-scope preapproval reached the native SSH client")
		}

		exactScope, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
			TargetID: target.ID, OperationID: preapprovedOperation.ID,
			Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"q01-approved-namespace"`)},
		})
		if err != nil || exactScope.Status != "succeeded" || exactScope.RemoteOutcome != SSHRemoteOutcomeConfirmed {
			t.Fatal("exact-scope preapproved operation did not complete through the native client")
		}
		if got := server.takeCommand(t); got != "q01-native-write namespace=q01-approved-namespace" {
			t.Fatal("preapproved operation did not send its fixed command and exact parameter")
		}
		if approver.calls.Load() != 0 {
			t.Fatal("exact-scope preapproval unexpectedly requested a per-call prompt")
		}

		denyingApprover := &q01NativeApprover{}
		denyingController := q01NativeController(target, runner, denyingApprover)
		denied, err := denyingController.RunSSHOperation(context.Background(), SSHOperationInput{
			TargetID: target.ID, OperationID: perCallOperation.ID,
			Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"q01-denied-namespace"`)},
		})
		if err != nil || denied.Status != "denied" || denied.RemoteOutcome != SSHRemoteOutcomeNotNeeded || runner.starts.Load() != 2 || server.execCount.Load() != 2 {
			t.Fatal("denied per-call operation reached the native SSH client")
		}
		if denyingApprover.calls.Load() != 1 {
			t.Fatal("per-call denial did not request exactly one approval")
		}

		perCall, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
			TargetID: target.ID, OperationID: perCallOperation.ID,
			Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"q01-confirmed-namespace"`)},
		})
		if err != nil || perCall.Status != "succeeded" || perCall.RemoteOutcome != SSHRemoteOutcomeConfirmed {
			t.Fatal("approved per-call operation did not complete through the native client")
		}
		if got := server.takeCommand(t); got != "q01-native-write namespace=q01-confirmed-namespace" {
			t.Fatal("per-call operation did not send its fixed command and approved parameter")
		}
		if approver.calls.Load() != 1 {
			t.Fatal("state-changing per-call operation did not request exactly one approval")
		}
		if runner.starts.Load() != 3 || server.execCount.Load() != 3 || !runner.allProcessesClosed() {
			t.Fatal("native OpenSSH process count or closure state was unexpected")
		}
	})

	t.Run("unknown and changed host keys fail closed without trust updates", func(t *testing.T) {
		for _, changed := range []bool{false, true} {
			name := "unknown host key"
			if changed {
				name = "changed host key"
			}
			t.Run(name, func(t *testing.T) {
				serverSigner := q01NativeNewSigner(t)
				server := q01NativeStartServer(t, q01NativeServerOptions{
					hostSigner:    serverSigner,
					authorizedKey: clientPublicKey,
				})
				knownHostsPath := filepath.Join(privateDir, "trust check known_hosts")
				initial := []byte{}
				if changed {
					otherSigner := q01NativeNewSigner(t)
					initial = q01NativeKnownHostsBytes(server.address(), otherSigner.PublicKey())
				}
				if err := os.WriteFile(knownHostsPath, initial, 0600); err != nil {
					t.Fatal("could not write private trust fixture")
				}
				configPath := q01NativeWriteSSHConfig(t, privateDir, "trust check ssh config", knownHostsPath, server.port())
				runner := q01NativeNewRunner(sshPath, configPath, clientKeyPath)
				operation := q01NativeReadOperation()
				target := q01NativeTarget(operation)
				controller := q01NativeController(target, runner, &q01NativeApprover{approved: true})
				result, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
					TargetID: target.ID, OperationID: operation.ID,
				})
				if err != nil || result.Status != "unknown" || result.RemoteOutcome != SSHRemoteOutcomeUnknown {
					t.Fatal("untrusted host key did not produce a safe unknown result")
				}
				after, readErr := os.ReadFile(knownHostsPath)
				if readErr != nil || string(after) != string(initial) {
					t.Fatal("host trust failure changed the private known_hosts fixture")
				}
				if runner.starts.Load() != 1 || server.execCount.Load() != 0 {
					t.Fatal("untrusted host key reached a remote exec request or retried")
				}
				if !runner.allProcessesClosed() {
					t.Fatal("native OpenSSH process remained open after host trust failure")
				}
			})
		}
	})

	t.Run("cancel after exec leaves remote outcome unknown and does not retry", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server := q01NativeStartServer(t, q01NativeServerOptions{
			authorizedKey: clientPublicKey,
			holdAfterExec: true,
			onExec:        cancel,
		})
		knownHostsPath := filepath.Join(privateDir, "cancel known_hosts")
		q01NativeWriteKnownHosts(t, knownHostsPath, server.address(), server.hostPublicKey)
		configPath := q01NativeWriteSSHConfig(t, privateDir, "cancel ssh config", knownHostsPath, server.port())
		runner := q01NativeNewRunner(sshPath, configPath, clientKeyPath)
		operation := q01NativePerCallOperation()
		target := q01NativeTarget(operation)
		controller := q01NativeController(target, runner, &q01NativeApprover{approved: true})
		input := SSHOperationInput{
			TargetID: target.ID, OperationID: operation.ID,
			Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"q01-cancelled-change"`)},
		}
		result, err := controller.RunSSHOperation(cancelCtx, input)
		if err != nil || result.Status != "unknown" || result.RemoteOutcome != SSHRemoteOutcomeUnknown || !strings.Contains(result.NextAction, "do not automatically retry") {
			t.Fatal("cancelled native SSH operation did not report an unknown remote outcome without retry guidance")
		}
		if got := server.takeCommand(t); got != "q01-native-write namespace=q01-cancelled-change" {
			t.Fatal("cancelled session did not reach the single approved fixed command")
		}
		if runner.starts.Load() != 1 || server.execCount.Load() != 1 || !runner.allProcessesClosed() {
			t.Fatal("cancelled SSH command retried or left the local process open")
		}
		select {
		case <-server.clientChannelClosed:
		case <-time.After(5 * time.Second):
			t.Fatal("loopback SSH server did not observe local channel closure after cancellation")
		}
	})
}

// runtimeGOOS keeps the opt-in test self-contained without importing runtime
// into production files or relying on a shell environment's platform label.
func runtimeGOOS() string {
	return runtime.GOOS
}

type q01NativeRunner struct {
	delegate *openSSHCommandRunner
	starts   atomic.Int32
	mu       sync.Mutex
	process  []*sshExecProcess
}

func q01NativeNewRunner(path, configPath, identityPath string) *q01NativeRunner {
	return &q01NativeRunner{delegate: &openSSHCommandRunner{
		path:                    path,
		privateTestConfigFile:   configPath,
		privateTestIdentityFile: identityPath,
	}}
}

func (r *q01NativeRunner) Start(ctx context.Context, alias, command string) (sshRunningProcess, error) {
	process, err := r.delegate.Start(ctx, alias, command)
	if err != nil {
		return nil, err
	}
	concrete, ok := process.(*sshExecProcess)
	if !ok {
		return nil, errSSHRunnerUnavailable
	}
	r.starts.Add(1)
	r.mu.Lock()
	r.process = append(r.process, concrete)
	r.mu.Unlock()
	return concrete, nil
}

func (r *q01NativeRunner) allProcessesClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.process) != int(r.starts.Load()) {
		return false
	}
	for _, process := range r.process {
		if process.command.ProcessState == nil || !process.command.ProcessState.Exited() {
			return false
		}
	}
	return true
}

type q01NativeApprover struct {
	approved bool
	calls    atomic.Int32
}

func (a *q01NativeApprover) Confirm(context.Context, sshApprovalRequest) (bool, error) {
	a.calls.Add(1)
	return a.approved, nil
}

type q01NativeServerOptions struct {
	hostSigner    ssh.Signer
	authorizedKey ssh.PublicKey
	holdAfterExec bool
	onExec        func()
}

type q01NativeServer struct {
	listener            net.Listener
	hostPublicKey       ssh.PublicKey
	options             q01NativeServerOptions
	commands            chan string
	clientChannelClosed chan struct{}
	execCount           atomic.Int32
	mu                  sync.Mutex
	connections         map[net.Conn]struct{}
	closed              chan struct{}
	closeOnce           sync.Once
	closeDone           chan struct{}
	workersStopped      bool
	workers             sync.WaitGroup
}

func q01NativeStartServer(t *testing.T, options q01NativeServerOptions) *q01NativeServer {
	t.Helper()
	if options.hostSigner == nil {
		options.hostSigner = q01NativeNewSigner(t)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not open the owned loopback SSH listener")
	}
	server := &q01NativeServer{
		listener:            listener,
		hostPublicKey:       options.hostSigner.PublicKey(),
		options:             options,
		commands:            make(chan string, 4),
		clientChannelClosed: make(chan struct{}),
		connections:         make(map[net.Conn]struct{}),
		closed:              make(chan struct{}),
		closeDone:           make(chan struct{}),
	}
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if metadata.User() != "q01-native-user" || options.authorizedKey == nil || !bytesEqual(key.Marshal(), options.authorizedKey.Marshal()) {
			return nil, errQ01NativeAuthentication
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(options.hostSigner)
	server.workers.Add(1)
	go server.acceptLoop(serverConfig)
	t.Cleanup(func() {
		if !server.close() {
			t.Error("owned loopback SSH listener workers did not stop")
		}
	})
	return server
}

func (s *q01NativeServer) acceptLoop(config *ssh.ServerConfig) {
	defer s.workers.Done()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				return
			}
		}
		s.mu.Lock()
		s.connections[connection] = struct{}{}
		s.mu.Unlock()
		s.workers.Add(1)
		go s.serveConnection(connection, config)
	}
}

func (s *q01NativeServer) serveConnection(connection net.Conn, config *ssh.ServerConfig) {
	defer s.workers.Done()
	defer func() {
		_ = connection.Close()
		s.mu.Lock()
		delete(s.connections, connection)
		s.mu.Unlock()
	}()
	_ = connection.SetDeadline(time.Now().Add(15 * time.Second))
	serverConnection, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	globalRequestsDone := make(chan struct{})
	go func() {
		ssh.DiscardRequests(requests)
		close(globalRequestsDone)
	}()
	defer func() {
		_ = serverConnection.Close()
		<-globalRequestsDone
	}()
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			return
		}
		for request := range channelRequests {
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			var payload struct{ Command string }
			if ssh.Unmarshal(request.Payload, &payload) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			s.execCount.Add(1)
			s.commands <- payload.Command
			_ = request.Reply(true, nil)
			if s.options.onExec != nil {
				s.options.onExec()
			}
			if s.options.holdAfterExec {
				_, _ = io.Copy(io.Discard, channel)
				_ = channel.Close()
				close(s.clientChannelClosed)
				return
			}
			_, _ = channel.Write([]byte("Q01_NATIVE_SSH_OK\n"))
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
			_ = channel.Close()
			return
		}
		_ = channel.Close()
	}
}

func (s *q01NativeServer) close() bool {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.listener.Close()
		s.mu.Lock()
		for connection := range s.connections {
			_ = connection.Close()
		}
		s.mu.Unlock()
		done := make(chan struct{})
		go func() {
			s.workers.Wait()
			close(done)
		}()
		select {
		case <-done:
			s.workersStopped = true
		case <-time.After(5 * time.Second):
		}
		close(s.closeDone)
	})
	<-s.closeDone
	return s.workersStopped
}

func (s *q01NativeServer) address() string { return s.listener.Addr().String() }

func (s *q01NativeServer) port() int {
	_, portText, _ := net.SplitHostPort(s.address())
	port, _ := strconv.Atoi(portText)
	return port
}

func (s *q01NativeServer) takeCommand(t *testing.T) string {
	t.Helper()
	select {
	case command := <-s.commands:
		return command
	case <-time.After(8 * time.Second):
		t.Fatal("loopback SSH server did not receive the fixed command")
		return ""
	}
}

func q01NativeNewSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("could not generate an ephemeral SSH key")
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal("could not create an ephemeral SSH signer")
	}
	return signer
}

func q01NativeWriteClientKey(t *testing.T, privateDir string) (ssh.Signer, string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("could not generate the ephemeral client key")
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "q01 ephemeral native QA key")
	if err != nil {
		t.Fatal("could not encode the ephemeral client key")
	}
	keyPath := filepath.Join(privateDir, "q01 client key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal("could not store the ephemeral client key in the restricted directory")
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal("could not create the ephemeral client signer")
	}
	return signer, keyPath
}

func q01NativeWriteKnownHosts(t *testing.T, path, address string, key ssh.PublicKey) {
	t.Helper()
	if err := os.WriteFile(path, q01NativeKnownHostsBytes(address, key), 0600); err != nil {
		t.Fatal("could not write the private known_hosts fixture")
	}
}

func q01NativeKnownHostsBytes(address string, key ssh.PublicKey) []byte {
	normalized := knownhosts.Normalize(address)
	return []byte(knownhosts.Line([]string{normalized}, key) + "\n")
}

func q01NativeWriteSSHConfig(t *testing.T, privateDir, name, knownHostsPath string, port int) string {
	t.Helper()
	config := strings.Join([]string{
		"Host q01-native-loopback",
		"    HostName 127.0.0.1",
		"    Port " + strconv.Itoa(port),
		"    User q01-native-user",
		"    BatchMode yes",
		"    StrictHostKeyChecking yes",
		"    UserKnownHostsFile " + q01NativeConfigPath(knownHostsPath),
		"    GlobalKnownHostsFile none",
		"    UpdateHostKeys no",
		"    CheckHostIP no",
		"    VerifyHostKeyDNS no",
		"    IdentityAgent none",
		"    IdentitiesOnly yes",
		"    PreferredAuthentications publickey",
		"    PasswordAuthentication no",
		"    KbdInteractiveAuthentication no",
		"    ForwardAgent no",
		"    ClearAllForwardings yes",
		"    ControlMaster no",
		"    ControlPath none",
		"    ConnectTimeout 3",
		"    ConnectionAttempts 1",
		"",
	}, "\n")
	path := filepath.Join(privateDir, name)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal("could not write the private OpenSSH client config")
	}
	return path
}

func q01NativeConfigPath(path string) string {
	return `"` + strings.ReplaceAll(filepath.ToSlash(path), `"`, `\"`) + `"`
}

func q01NativeRestrictDirectory(t *testing.T, path string) {
	t.Helper()
	whoami, err := exec.Command("whoami.exe", "/user", "/fo", "csv", "/nh").Output()
	if err != nil {
		t.Fatal("could not identify the current-user SID for private test-file ACLs")
	}
	row, err := csv.NewReader(strings.NewReader(string(whoami))).Read()
	if err != nil || len(row) != 2 {
		t.Fatal("could not parse the current-user SID for private test-file ACLs")
	}
	sid := strings.TrimSpace(row[1])
	if !regexp.MustCompile(`^S-1-5-21-(?:[0-9]+-){2,}[0-9]+$`).MatchString(sid) {
		t.Fatal("the private test-file ACL did not resolve to a current-user SID")
	}
	acl := "*" + sid + ":(OI)(CI)F"
	if err := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", acl, "/Q").Run(); err != nil {
		t.Fatal("could not restrict the private test-material directory to the current user")
	}
	if !q01NativeVerifyCurrentUserACL(path, sid) {
		t.Fatal("private test-material directory ACL was not limited to the current user")
	}
}

func q01NativeVerifyCurrentUserACL(path, sidText string) bool {
	currentSID, err := windows.StringToSid(sidText)
	if err != nil {
		return false
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return false
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return false
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return false
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !currentSID.Equals(aceSID) || ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
		return false
	}
	return ace.Mask&windows.STANDARD_RIGHTS_ALL == windows.STANDARD_RIGHTS_ALL && ace.Mask&0x1ff == 0x1ff
}

var errQ01NativeAuthentication = &q01NativeAuthError{}

type q01NativeAuthError struct{}

func (*q01NativeAuthError) Error() string { return "synthetic native test authentication failed" }

func q01NativeReadOperation() SSHOperationDefinition {
	operation, err := normalizeSSHOperationDefinition(SSHOperationDefinition{
		ID: "q01_native_read", Name: "Native fixed read", Summary: "Return a constant loopback response.",
		Program: "q01-native-read", FixedArgs: []string{"status"}, Risk: SSHRiskReadOnly,
		Approval: OperationApprovalPolicy{Mode: OperationApprovalReadOnly},
	}, "q01_native_target")
	if err != nil {
		panic("invalid built-in native QA read operation")
	}
	return operation
}

func q01NativePreapprovedOperation() SSHOperationDefinition {
	return q01NativeWriteOperation("q01_native_preapproved", OperationApprovalPreapproved, "q01-approved-namespace")
}

func q01NativePerCallOperation() SSHOperationDefinition {
	return q01NativeWriteOperation("q01_native_per_call", OperationApprovalPerCall, "")
}

func q01NativeWriteOperation(id string, mode OperationApprovalMode, scopeValue string) SSHOperationDefinition {
	operation := SSHOperationDefinition{
		ID: id, Name: "Native fixed write", Summary: "Record a fixed loopback request without executing it.",
		Program: "q01-native-write", FixedArgs: []string{"namespace={{namespace}}"},
		Parameters: []SSHParameterSpec{{Name: "namespace", Type: "string", Required: true}},
		Risk:       SSHRiskStateChanging, Approval: OperationApprovalPolicy{Mode: mode},
	}
	if mode == OperationApprovalPreapproved {
		operation.Approval.Scope = []string{sshApprovalScopeFingerprint("q01_native_target", "q01-native-loopback", id, map[string]string{"namespace": scopeValue})}
	}
	normalized, err := normalizeSSHOperationDefinition(operation, "q01_native_target")
	if err != nil {
		panic("invalid built-in native QA write operation")
	}
	return normalized
}

func q01NativeTarget(operations ...SSHOperationDefinition) sshTargetDefinition {
	return sshTargetDefinition{
		ID: "q01_native_target", Name: "Q01 Loopback SSH Target", Alias: "q01-native-loopback",
		Operations: operations,
	}
}

func q01NativeController(target sshTargetDefinition, runner sshOperationRunner, approver sshOperationApprover) *sshOperationController {
	store := newFakeSSHOperationStore()
	store.snapshot = sshOperationConfigSnapshot{Revision: "q01_native_snapshot", Targets: []sshTargetDefinition{cloneSSHTarget(target)}}
	return newSSHOperationController(store, runner, approver)
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
