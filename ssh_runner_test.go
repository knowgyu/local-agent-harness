package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeSSHOperationRunner struct {
	mu       sync.Mutex
	result   sshProcessResult
	startErr error
	starts   int
	alias    string
	command  string
}

func (r *fakeSSHOperationRunner) Start(_ context.Context, alias, command string) (sshRunningProcess, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	r.alias = alias
	r.command = command
	if r.startErr != nil {
		return nil, r.startErr
	}
	result := r.result
	result.Stdout = append([]byte(nil), r.result.Stdout...)
	result.Stderr = append([]byte(nil), r.result.Stderr...)
	return &fakeSSHRunningProcess{result: result}, nil
}

type fakeSSHRunningProcess struct {
	result sshProcessResult
}

func (p *fakeSSHRunningProcess) Wait() sshProcessResult {
	return p.result
}

type fakeSSHOperationApprover struct {
	approved  bool
	err       error
	calls     int
	request   sshApprovalRequest
	onConfirm func()
}

func (a *fakeSSHOperationApprover) Confirm(_ context.Context, request sshApprovalRequest) (bool, error) {
	a.calls++
	a.request = request
	if a.onConfirm != nil {
		a.onConfirm()
	}
	return a.approved, a.err
}

func TestSSHOperationBuildsOnlyRegisteredCommandAndSafeTypedParameters(t *testing.T) {
	operation := stateChangingSSHOperation("target_one", "qa-host", map[string]string{"namespace": "qa"})
	prepared, err := prepareSSHInvocation("target_one", "qa-host", operation, map[string]json.RawMessage{"namespace": json.RawMessage(`"qa"`)})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.command != "service-tool restart qa" {
		t.Fatalf("unexpected fixed command expansion: %q", prepared.command)
	}
	if prepared.scope != sshApprovalScopeFingerprint("target_one", "qa-host", operation.ID, map[string]string{"namespace": "qa"}) {
		t.Fatal("scope fingerprint did not use normalized parameter data")
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`"qa; rm -rf /"`), json.RawMessage(`"--help"`), json.RawMessage(`[]`), json.RawMessage(`null`),
		json.RawMessage(`"ghp_0123456789abcdefghijklmnopqrstuvwxyzABCD"`), json.RawMessage(`"https://10.0.0.2/admin"`),
		json.RawMessage(`"10.0.0.2"`), json.RawMessage(`"example.internal"`), json.RawMessage(`"service:8443"`),
		json.RawMessage(`"/etc/passwd"`), json.RawMessage(`"2130706433"`),
	} {
		if _, err := prepareSSHInvocation("target_one", "qa-host", operation, map[string]json.RawMessage{"namespace": raw}); err == nil {
			t.Fatalf("unsafe parameter was accepted: %s", raw)
		}
	}
	if validSSHRemoteCommand("kubectl get pods; whoami") || validSSHRemoteCommand("kubectl $(whoami)") || !validSSHRemoteCommand("kubectl get pods --namespace=qa") {
		t.Fatal("remote command portable-token check did not reject shell syntax")
	}
}

func TestSSHOperationReadOnlyUsesExistingAliasAndDoesNotApproveEachRun(t *testing.T) {
	operation := readOnlySSHOperation()
	target := sshTargetDefinition{ID: "target_one", Name: "QA", Alias: "qa-host", Operations: []SSHOperationDefinition{operation}}
	runner := &fakeSSHOperationRunner{result: sshProcessResult{Stdout: []byte("pod-1\n"), ExitKnown: true}}
	approver := &fakeSSHOperationApprover{}
	controller, _ := newTestSSHController(target, runner, approver)

	result, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: operation.ID,
	})
	if err != nil || result.Status != "succeeded" || result.RemoteOutcome != SSHRemoteOutcomeConfirmed || result.Output != "stdout:\npod-1\n" {
		t.Fatalf("unexpected read-only run result: %#v err=%v", result, err)
	}
	if runner.starts != 1 || runner.alias != "qa-host" || runner.command != "kubectl get pods --namespace=qa" {
		t.Fatalf("runner did not use the stored alias and operation: starts=%d alias=%q command=%q", runner.starts, runner.alias, runner.command)
	}
	if approver.calls != 0 {
		t.Fatalf("read-only operation prompted for approval %d times", approver.calls)
	}
}

func TestSSHOperationPreapprovalIsBoundToExactRevisionAndScope(t *testing.T) {
	operation := stateChangingSSHOperation("target_one", "dev-host", map[string]string{"namespace": "dev"})
	target := sshTargetDefinition{ID: "target_one", Name: "Dev", Alias: "dev-host", Operations: []SSHOperationDefinition{operation}}
	runner := &fakeSSHOperationRunner{result: sshProcessResult{ExitKnown: true}}
	approver := &fakeSSHOperationApprover{}
	controller, _ := newTestSSHController(target, runner, approver)

	matching, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: operation.ID,
		Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"dev"`)},
	})
	if err != nil || matching.Status != "succeeded" || approver.calls != 0 {
		t.Fatalf("exact preapproval did not run without another prompt: result=%#v calls=%d err=%v", matching, approver.calls, err)
	}
	changed, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: operation.ID,
		Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"prod"`)},
	})
	if err != nil || changed.Status != "rejected" || changed.RemoteOutcome != SSHRemoteOutcomeNotNeeded || runner.starts != 1 {
		t.Fatalf("out-of-scope invocation reached SSH: result=%#v starts=%d err=%v", changed, runner.starts, err)
	}
}

func TestSSHOperationDestructiveAlwaysUsesOneCallApprovalAndRechecksRevision(t *testing.T) {
	operation, err := normalizeSSHOperationDefinition(SSHOperationDefinition{
		ID: "delete_pod", Name: "Delete pod", Summary: "Remove one reviewed pod.", Program: "kubectl",
		FixedArgs:  []string{"delete", "pod", "{{pod}}"},
		Parameters: []SSHParameterSpec{{Name: "pod", Type: "string", Required: true}},
		Risk:       SSHRiskDestructive, Approval: OperationApprovalPolicy{Mode: OperationApprovalPerCall},
	}, "target_one")
	if err != nil {
		t.Fatal(err)
	}
	target := sshTargetDefinition{ID: "target_one", Name: "Prod", Alias: "prod-host", Operations: []SSHOperationDefinition{operation}}
	store := newFakeSSHOperationStore()
	store.snapshot = sshOperationConfigSnapshot{Revision: "r1", Targets: []sshTargetDefinition{target}}
	runner := &fakeSSHOperationRunner{result: sshProcessResult{ExitKnown: true}}
	approver := &fakeSSHOperationApprover{approved: true}
	controller := newSSHOperationController(store, runner, approver)
	approver.onConfirm = func() {
		store.mu.Lock()
		store.snapshot.Targets[0].Name = "Changed after review"
		store.snapshot.Revision = "r2"
		store.mu.Unlock()
	}

	result, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: operation.ID,
		Parameters: map[string]json.RawMessage{"pod": json.RawMessage(`"pod-7"`)},
	})
	if err != nil || result.Status != "stale" || result.RemoteOutcome != SSHRemoteOutcomeNotNeeded || runner.starts != 0 || approver.calls != 1 {
		t.Fatalf("changed reviewed target was executed: result=%#v starts=%d approvals=%d err=%v", result, runner.starts, approver.calls, err)
	}
}

func TestSSHOperationCancellationTimeoutAndTransportErrorHaveUnknownOutcomeWithoutRetry(t *testing.T) {
	operation := stateChangingSSHOperation("target_one", "dev-host", map[string]string{"namespace": "dev"})
	target := sshTargetDefinition{ID: "target_one", Name: "Dev", Alias: "dev-host", Operations: []SSHOperationDefinition{operation}}
	for _, test := range []struct {
		name   string
		result sshProcessResult
	}{
		{name: "cancelled after start", result: sshProcessResult{Canceled: true}},
		{name: "timed out after start", result: sshProcessResult{TimedOut: true}},
		{name: "host trust or authentication error", result: sshProcessResult{ExitCode: 255}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeSSHOperationRunner{result: test.result}
			controller, _ := newTestSSHController(target, runner, &fakeSSHOperationApprover{})
			result, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
				TargetID: target.ID, OperationID: operation.ID,
				Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"dev"`)},
			})
			if err != nil || result.Status != "unknown" || result.RemoteOutcome != SSHRemoteOutcomeUnknown || runner.starts != 1 {
				t.Fatalf("remote outcome was not made unknown exactly once: result=%#v starts=%d err=%v", result, runner.starts, err)
			}
		})
	}
}

func TestSSHOperationApprovalDenialAndLocalStartFailureNeverClaimRemoteExecution(t *testing.T) {
	operation, err := normalizeSSHOperationDefinition(SSHOperationDefinition{
		ID: "delete_pod", Name: "Delete pod", Summary: "Remove one reviewed pod.", Program: "kubectl",
		FixedArgs:  []string{"delete", "pod", "{{pod}}"},
		Parameters: []SSHParameterSpec{{Name: "pod", Type: "string", Required: true}},
		Risk:       SSHRiskDestructive, Approval: OperationApprovalPolicy{Mode: OperationApprovalPerCall},
	}, "target_one")
	if err != nil {
		t.Fatal(err)
	}
	target := sshTargetDefinition{ID: "target_one", Name: "Prod", Alias: "prod-host", Operations: []SSHOperationDefinition{operation}}
	input := SSHOperationInput{TargetID: target.ID, OperationID: operation.ID, Parameters: map[string]json.RawMessage{"pod": json.RawMessage(`"pod-7"`)}}

	runner := &fakeSSHOperationRunner{result: sshProcessResult{ExitKnown: true}}
	controller, _ := newTestSSHController(target, runner, &fakeSSHOperationApprover{approved: false})
	denied, err := controller.RunSSHOperation(context.Background(), input)
	if err != nil || denied.Status != "denied" || denied.RemoteOutcome != SSHRemoteOutcomeNotNeeded || runner.starts != 0 {
		t.Fatalf("denial reached SSH or claimed remote work: result=%#v starts=%d err=%v", denied, runner.starts, err)
	}

	changing := stateChangingSSHOperation("target_one", "dev-host", map[string]string{"namespace": "dev"})
	target = sshTargetDefinition{ID: "target_one", Name: "Dev", Alias: "dev-host", Operations: []SSHOperationDefinition{changing}}
	runner = &fakeSSHOperationRunner{startErr: errors.New("private ssh config path")}
	controller, _ = newTestSSHController(target, runner, &fakeSSHOperationApprover{})
	started, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: changing.ID,
		Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"dev"`)},
	})
	if err != nil || started.Status != "not_started" || started.RemoteOutcome != SSHRemoteOutcomeNotNeeded || runner.starts != 1 || strings.Contains(started.NextAction, "private ssh config path") {
		t.Fatalf("local start failure was misreported or leaked: result=%#v starts=%d err=%v", started, runner.starts, err)
	}
}

func TestSSHOperationOutputIsBoundedRedactedAndFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		process  sshProcessResult
		contains string
		wantSafe bool
	}{
		{name: "credential field redacted", process: sshProcessResult{Stdout: []byte("password=canary-value\n"), ExitKnown: true}, contains: "[REDACTED]", wantSafe: true},
		{name: "invalid utf8 withheld", process: sshProcessResult{Stdout: []byte{0xff}, ExitKnown: true}, wantSafe: false},
		{name: "nul withheld", process: sshProcessResult{Stdout: []byte("before\x00after"), ExitKnown: true}, wantSafe: false},
		{name: "truncated streams bounded", process: sshProcessResult{Stdout: bytes.Repeat([]byte("x"), maxSSHStreamOutputBytes), StdoutTruncated: true, OutputTruncated: true, ExitKnown: true}, wantSafe: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, truncated, safe := sanitizeSSHOperationOutput(test.process)
			if safe != test.wantSafe {
				t.Fatalf("safe=%v want %v output=%q", safe, test.wantSafe, output)
			}
			if test.contains != "" && !strings.Contains(output, test.contains) {
				t.Fatalf("expected filtered marker absent: %q", output)
			}
			if strings.Contains(output, "canary-value") {
				t.Fatal("raw credential canary remained in output")
			}
			if len(output) > maxSSHOperationOutputBytes || (test.process.OutputTruncated && !truncated) {
				t.Fatalf("output cap or truncation state is wrong: len=%d truncated=%v", len(output), truncated)
			}
		})
	}
}

func TestOpenSSHArgumentPlanDoesNotDisableUserHostTrust(t *testing.T) {
	args := openSSHArguments("existing-alias", "docker ps")
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-T", "BatchMode=yes", "ForwardAgent=no", "ClearAllForwardings=yes", "existing-alias", "docker ps"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("OpenSSH arguments omitted %q: %v", expected, args)
		}
	}
	for _, forbidden := range []string{"StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null", "-A"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("OpenSSH arguments weakened or forwarded user trust: %v", args)
		}
	}
}

func newTestSSHController(target sshTargetDefinition, runner *fakeSSHOperationRunner, approver *fakeSSHOperationApprover) (*sshOperationController, *fakeSSHOperationStore) {
	store := newFakeSSHOperationStore()
	store.snapshot = sshOperationConfigSnapshot{Revision: "test-revision", Targets: []sshTargetDefinition{cloneSSHTarget(target)}}
	return newSSHOperationController(store, runner, approver), store
}

func stateChangingSSHOperation(targetID, targetAlias string, parameters map[string]string) SSHOperationDefinition {
	operation := SSHOperationDefinition{
		ID: "restart_service", Name: "Restart service", Summary: "Restart one explicitly selected service.",
		Program: "service-tool", FixedArgs: []string{"restart", "{{namespace}}"},
		Parameters: []SSHParameterSpec{{Name: "namespace", Type: "string", Required: true}},
		Risk:       SSHRiskStateChanging,
	}
	resolved := make(map[string]string, len(parameters))
	for key, value := range parameters {
		resolved[key] = value
	}
	operation.Approval = OperationApprovalPolicy{
		Mode:  OperationApprovalPreapproved,
		Scope: []string{sshApprovalScopeFingerprint(targetID, targetAlias, operation.ID, resolved)},
	}
	normalized, err := normalizeSSHOperationDefinition(operation, targetID)
	if err != nil {
		panic(err)
	}
	return normalized
}
