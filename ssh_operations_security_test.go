package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestSSHPreapprovalScopeIsBoundToTargetAlias(t *testing.T) {
	operation := stateChangingSSHOperation("target_one", "dev-host", map[string]string{"namespace": "dev"})
	target := sshTargetDefinition{ID: "target_one", Name: "Dev", Alias: "dev-host", Operations: []SSHOperationDefinition{operation}}
	runner := &fakeSSHOperationRunner{result: sshProcessResult{ExitKnown: true}}
	controller, store := newTestSSHController(target, runner, &fakeSSHOperationApprover{})

	target.Alias = "other-host"
	if _, _, err := controller.SaveTarget(context.Background(), target, "test-revision"); err != nil {
		t.Fatal(err)
	}
	result, err := controller.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: operation.ID,
		Parameters: map[string]json.RawMessage{"namespace": json.RawMessage(`"dev"`)},
	})
	if err != nil || result.Status != "rejected" || runner.starts != 0 {
		t.Fatalf("preapproval survived target alias change: result=%#v starts=%d err=%v", result, runner.starts, err)
	}
	if len(store.snapshot.Targets) != 1 || store.snapshot.Targets[0].Alias != "other-host" {
		t.Fatalf("test did not persist the alias change: %#v", store.snapshot.Targets)
	}
}

func TestSSHStringParametersRejectEndpointValuesEvenUnderOpaqueNames(t *testing.T) {
	spec := SSHParameterSpec{Name: "resource", Type: "string", Required: true}
	for _, value := range []string{
		"https://10.0.0.2/admin", "https://example.invalid", "example.internal", "10.0.0.2",
		"2001:db8::1", "server:8443", "/etc/passwd", "localhost", "2130706433",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseSSHParameter(spec, json.RawMessage(mustJSONQuote(value))); !errors.Is(err, errSSHOperationRejected) {
				t.Fatalf("endpoint-like value accepted for opaque parameter name: %q err=%v", value, err)
			}
		})
	}
	if got, err := parseSSHParameter(spec, json.RawMessage(`"qa-1"`)); err != nil || got != "qa-1" {
		t.Fatalf("bounded identifier was rejected: got=%q err=%v", got, err)
	}
}

func TestSSHEndpointParameterNamesAndReadOnlyDynamicParametersAreRejected(t *testing.T) {
	for _, name := range []string{"host", "hostname", "url", "endpoint", "server", "command", "path"} {
		t.Run(name, func(t *testing.T) {
			operation := SSHOperationDefinition{
				ID: "safe_op", Name: "Safe operation", Summary: "One fixed action.", Program: "fixed-tool",
				FixedArgs:  []string{"inspect", "{{" + name + "}}"},
				Parameters: []SSHParameterSpec{{Name: name, Type: "string", Required: true}},
				Risk:       SSHRiskStateChanging, Approval: OperationApprovalPolicy{Mode: OperationApprovalPerCall},
			}
			if _, err := normalizeSSHOperationDefinition(operation, "target_one"); !errors.Is(err, errSSHDefinitionInvalid) {
				t.Fatalf("endpoint-like parameter name was accepted: %q err=%v", name, err)
			}
		})
	}

	operation := SSHOperationDefinition{
		ID: "read_dynamic", Name: "Read dynamic", Summary: "Read a selected item.", Program: "fixed-tool",
		FixedArgs:  []string{"inspect", "{{item}}"},
		Parameters: []SSHParameterSpec{{Name: "item", Type: "string", Required: true}},
		Risk:       SSHRiskReadOnly, Approval: OperationApprovalPolicy{Mode: OperationApprovalReadOnly},
	}
	if _, err := normalizeSSHOperationDefinition(operation, "target_one"); !errors.Is(err, errSSHDefinitionInvalid) {
		t.Fatalf("parameterized read-only operation was accepted: %v", err)
	}
}

func mustJSONQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
