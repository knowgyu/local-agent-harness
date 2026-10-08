package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type sshProcessResult struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	OutputTruncated bool
	ExitCode        int
	ExitKnown       bool
	TimedOut        bool
	Canceled        bool
}

type openSSHCommandRunner struct {
	path string

	// These private paths are populated only by same-package native acceptance
	// tests. Production construction leaves them empty; they are never persisted
	// or exposed through the UI or MCP request types.
	privateTestConfigFile   string
	privateTestIdentityFile string
}

type sshExecProcess struct {
	command *exec.Cmd
	ctx     context.Context
	stdout  *sshBoundedOutput
	stderr  *sshBoundedOutput
}

type sshBoundedOutput struct {
	mu        sync.Mutex
	value     []byte
	limit     int
	truncated bool
}

func newOpenSSHCommandRunner() *openSSHCommandRunner {
	return &openSSHCommandRunner{}
}

func (r *openSSHCommandRunner) Start(ctx context.Context, alias, remoteCommand string) (sshRunningProcess, error) {
	if ctx == nil || ctx.Err() != nil || !sshAliasPattern.MatchString(alias) || !validSSHRemoteCommand(remoteCommand) {
		return nil, errSSHRunnerUnavailable
	}
	path := r.path
	if path == "" {
		var err error
		path, err = resolveOpenSSHExecutable()
		if err != nil {
			return nil, errSSHRunnerUnavailable
		}
	}
	stdout := &sshBoundedOutput{limit: maxSSHStreamOutputBytes}
	stderr := &sshBoundedOutput{limit: maxSSHStreamOutputBytes}
	arguments := openSSHArguments(alias, remoteCommand)
	if r.privateTestConfigFile != "" {
		arguments = append([]string{"-F", r.privateTestConfigFile}, arguments...)
	}
	if r.privateTestIdentityFile != "" {
		arguments = append([]string{"-i", r.privateTestIdentityFile}, arguments...)
	}
	command := exec.CommandContext(ctx, path, arguments...)
	command.Stdin = nil
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = 2 * time.Second
	if err := command.Start(); err != nil {
		return nil, errSSHRunnerUnavailable
	}
	return &sshExecProcess{command: command, ctx: ctx, stdout: stdout, stderr: stderr}, nil
}

func (p *sshExecProcess) Wait() sshProcessResult {
	waitErr := p.command.Wait()
	stdout, stdoutTruncated := p.stdout.take()
	stderr, stderrTruncated := p.stderr.take()
	result := sshProcessResult{
		Stdout:          stdout,
		Stderr:          stderr,
		StdoutTruncated: stdoutTruncated,
		StderrTruncated: stderrTruncated,
		OutputTruncated: stdoutTruncated || stderrTruncated,
	}
	if p.ctx != nil && p.ctx.Err() != nil {
		result.TimedOut = errors.Is(p.ctx.Err(), context.DeadlineExceeded)
		result.Canceled = errors.Is(p.ctx.Err(), context.Canceled)
		return result
	}
	if waitErr == nil {
		result.ExitKnown = true
		result.ExitCode = 0
		return result
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		result.ExitCode = exitError.ExitCode()
		result.ExitKnown = result.ExitCode != 255
	}
	return result
}

func (w *sshBoundedOutput) Write(value []byte) (int, error) {
	if w == nil {
		return len(value), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.limit <= 0 || w.truncated {
		return len(value), nil
	}
	remaining := w.limit - len(w.value)
	if len(value) > remaining {
		if remaining > 0 {
			w.value = append(w.value, value[:remaining]...)
		}
		w.truncated = true
		return len(value), nil
	}
	w.value = append(w.value, value...)
	return len(value), nil
}

func (w *sshBoundedOutput) take() ([]byte, bool) {
	if w == nil {
		return nil, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	value := append([]byte(nil), w.value...)
	w.value = nil
	return value, w.truncated
}

func openSSHArguments(alias, remoteCommand string) []string {
	return []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
		alias,
		remoteCommand,
	}
}

func validSSHRemoteCommand(command string) bool {
	if command == "" || !utf8.ValidString(command) || len(command) > maxSSHOperationFixedArgs*257 {
		return false
	}
	for _, token := range strings.Fields(command) {
		if !sshArgumentTokenPattern.MatchString(token) {
			return false
		}
	}
	return true
}

var errSSHRunnerUnavailable = errors.New("The local OpenSSH client could not be started.")
