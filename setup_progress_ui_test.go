package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSetupProgressRequiresTargetReadEvidence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the setup progress UI check")
	}
	fragment, err := os.ReadFile("ui_fragments/readiness.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(fragment)
	start := strings.Index(source, "    function targetSetupProgress(")
	end := strings.Index(source, "    function renderTargets(")
	if start < 0 || end <= start {
		t.Fatal("setup progress function was not found")
	}
	command := exec.Command(node, "-e", source[start:end]+setupProgressChecks)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("setup progress evidence check: %v\n%s", err, output)
	}
}

const setupProgressChecks = `
const assert = require('node:assert/strict');
const base = {type: 'github', enabled: true, credential_state: 'reference_saved', first_read: {state: 'not_observed'}};
const state = value => targetSetupProgress({...base, ...value}).state;
assert.equal(state({}), 'setupNeedsTest');
assert.equal(state({connection_test: {state: 'success', source: 'saved_history'}}), 'setupNeedsRead');
assert.equal(state({connection_test: {state: 'failure', source: 'saved_history'}}), 'setupConnectionFailed');
assert.equal(state({connection_test: {state: 'success', source: 'user_reported'}}), 'setupNeedsTest');
assert.equal(state({first_read: {state: 'unavailable'}}), 'setupReadUnavailable');
assert.equal(state({first_read: undefined}), 'setupReadUnavailable');
const proof = {state: 'observed', source: 'mcp_observed', service: 'github', tool: 'github_repository', observed_at: new Date(Date.now() - 1000).toISOString()};
assert.equal(state({first_read: proof}), 'setupReadSucceeded');
for (const invalid of [
  {...proof, source: 'user_reported'},
  {...proof, service: 'jenkins'},
  {...proof, tool: ''},
  {...proof, observed_at: 'invalid'},
  {...proof, observed_at: new Date(Date.now() + 60000).toISOString()}
]) assert.equal(state({first_read: invalid}), 'setupReadUnavailable');
assert.equal(state({enabled: false, first_read: proof}), 'disabled');
assert.equal(state({credential_state: 'not_configured', first_read: proof}), 'noCredentialRef');
assert.equal(state({name: '<script>unsafe</script>'}), 'setupNeedsTest');
`
