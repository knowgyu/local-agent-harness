# Whole-repo Ponytail audit and onboarding change review — 2026-10-15

What repo does:
Local Agent Harness is a Windows-first MCP gateway for named service targets and approved operations. It stores settings and bounded first-read evidence locally.

Scope: Whole-repository audit with focused review of the onboarding/readiness change. The load assumption is one Windows user with multiple MCP server processes sharing local state; this review does not claim multi-user service capacity.

**Must fix:** The independent bounded reviewer found no remaining RC5 blocker once the annotation boundary below was documented.

**Should fix:** None identified for the stated load assumption. The bounded evidence history and client-attribution limits remain documented constraints.

## Findings and disposition

- First-read history keeps the newest 256 distinct combinations of service, tool, and target definition. New combinations can evict the oldest; when an absent receipt may have been evicted, readiness reports `unavailable`, not `not_observed`. A retained receipt confirms an MCP read but does not identify the calling client. See [onboarding_store.go](../../onboarding_store.go), [first_read_receipt.go](../../first_read_receipt.go), and [setup_status_test.go](../../setup_status_test.go).
- The readiness UI hides the new login auto-start action, which could be confused with unsupported HTTP client setup, while retaining the disable action for an already-enabled setting. The [RC5 release notes](../releases/v0.1.0-rc.5/RELEASE_NOTES.ko.md) document the change.
- Removed the unused `newSetupDraftResourceID` and `readinessNormalizeTargetActions` helpers. Moved the Python and SSH approval-route parsers into their test files; the existing tests remain. See [setup_draft.go](../../setup_draft.go), [readiness.go](../../readiness.go), [python_task_approval_test.go](../../python_task_approval_test.go), and [ssh_approval_test.go](../../ssh_approval_test.go).
- The setup guide is embedded at `lah://guides/agent-setup`; release documentation specifies a separate GitHub Release asset outside the portable ZIP. The ZIP builder refuses to overwrite an existing output directory. See [agent_setup_guide.go](../../agent_setup_guide.go), [Build-PortableRelease.ps1](../../packaging/windows/Build-PortableRelease.ps1), and [AGENT_SETUP.md](../AGENT_SETUP.md).
- MCP `readOnlyHint` describes intent not to mutate connected services or configured resources. A successful service read may still persist a bounded local receipt. The hint does not authorize a call or replace policy and approval checks. Default `setup_status` remains passive, with no network access or local writes; see the scope caveat in [SECURITY.md](../SECURITY.md) and [AGENT_SETUP.md](../AGENT_SETUP.md).

An independent bounded review (`/root/repo_audit`) found no remaining release blocker once the annotation boundary above was documented. That reviewer did not run tests.

## Validation and limits

The Windows-native Go suite passed with exit code 0 in 34.82 seconds; `go vet` passed with exit code 0 in 0.99 seconds.
The portable ZIP build passed in 20.65 seconds; two stripped EXEs were byte-identical and the 11-member ZIP privacy gates passed. Pinned Claude Desktop MCPB 2.1.2 validate/pack/info passed in 13.19 seconds and contained only `manifest.json` plus the server executable, matching the ZIP binary.
The final packaged EXE passed an isolated native stdio probe: protocol 2025-11-25, 23 tools, byte-identical embedded guide, passive `setup_status` with no file changes, and rejection of bad arguments. Windows PowerShell 5.1.26100.9444 and PowerShell 7.6.7 each passed the four lifecycle scripts and an exact-RC5 install/update/uninstall sequence (10/10 workflows total). The legacy script used RC5 in its RC4-named arguments; it tested RC3-to-current-RC5 recovery.

Client evidence: Codex CLI 0.162.1 (`gpt-6-luna`, `xhigh`) successfully called `github_repository` and `setup_status` against an isolated mock GitHub target; `first_read.state` was `observed`, `client_attribution` was `not_observed`, and the model final response was PASS. Claude Code 2.1.224 was blocked before tool use by HTTP 401 for expired OAuth, so no model ran. SDK fixtures passed for both clients but do not establish actual-client success.
Not checked: No real company services or Credential Manager storage/deletion were validated. The independent reviewer did not run tests.

Verdict: The independent bounded review found no remaining documentation/change blocker once the annotation boundary was documented; the recorded release validation passed.
