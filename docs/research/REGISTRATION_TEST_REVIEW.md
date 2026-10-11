# Registration test review — 2026-10-11

## Scope and result

This review covers the first URL parser and local preview route in `registration_url.go` and `registration_url_ui.go`, including the existing tests and the focused additions in `registration_regression_test.go`. The ordinary onboarding draft, temporary check, activation, and first-read receipt routes have separate end-to-end coverage in [`onboarding_acceptance_test.go`](../../onboarding_acceptance_test.go) and a cross-wave evidence matrix in [`ONBOARDING_VERIFICATION_2026-10-11.md`](ONBOARDING_VERIFICATION_2026-10-11.md).

No material production defect was demonstrated in the current URL parser or preview handler. The added regression tests pass, including with the race detector. The work strengthens URL boundaries, verifies preview against a real saved-settings fixture, and exercises concurrent requests through the local handler. It does not claim browser verification.

## Coverage reviewed

[`registration_url_test.go`](../../registration_url_test.go) already checks all three service kinds, GitHub origin/repository parsing, Jenkins job suffix inference and review fields, Dashboard proxy paths, unsafe URL forms, malformed hosts and ports, path traversal, encoded separators, and that parser errors omit the supplied URL.

[`registration_url_ui_test.go`](../../registration_url_ui_test.go) already exercises successful candidates through `newLocalUIHandler`, malformed forms, method/CSRF/Host/Origin checks, duplicate or unknown form fields, oversized bodies, safe error envelopes, and the preview route’s lack of credential-store access, settings mutation, and outbound requests.

[`registration_regression_test.go`](../../registration_regression_test.go) adds:

- `TestParseRegistrationURLPortAndLengthBoundaries`: ports 1 and 65535, rejection of zero/out-of-range/malformed ports, bracketed IPv6, and the 2048-byte URL limit.
- `TestParseRegistrationURLRejectsEncodedPathBoundariesForEveryService`: encoded and double-encoded separators, encoded NUL, and traversal checks across GitHub, Jenkins, and Dashboard paths.
- `TestHandleRegistrationURLPreviewPreservesExistingTargetsAndHistory`: a valid multi-service settings fixture with a saved GitHub target and connection history remains unchanged after a preview; it also checks `no-store` and `nosniff` response headers and confirms there is no credential-store or network access.
- `TestHandleRegistrationURLPreviewConcurrentRequestsKeepCandidatesIsolated`: simultaneous GitHub, Jenkins, and Dashboard requests return only their own candidate values.

Focused verification passed:

```text
go test -run 'Test(ParseRegistrationURLPortAndLengthBoundaries|ParseRegistrationURLRejectsEncodedPathBoundariesForEveryService|HandleRegistrationURLPreviewPreservesExistingTargetsAndHistory|HandleRegistrationURLPreviewConcurrentRequestsKeepCandidatesIsolated)$' .
ok   local-agent-harness  1.264s

go test -race -run 'Test(ParseRegistrationURLPortAndLengthBoundaries|ParseRegistrationURLRejectsEncodedPathBoundariesForEveryService|HandleRegistrationURLPreviewPreservesExistingTargetsAndHistory|HandleRegistrationURLPreviewConcurrentRequestsKeepCandidatesIsolated)$' .
ok   local-agent-harness  2.434s
```

## UI evidence boundary

The preview handler tests do not execute the inline JavaScript or establish browser state behavior. The UI implementation contains generation checks for late preview responses, invalidates candidates when the service/address changes, renders candidate strings as text, and asks for confirmation before switching a loaded saved-target form to a new target. Those observations are source review only in this audit.

The repository’s [`REGISTRATION_UI_IMPLEMENTATION.md`](REGISTRATION_UI_IMPLEMENTATION.md) records earlier functional CUA evidence for stale-candidate clearing, edit-to-new cancel/confirm, saved target preservation, cross-provider isolation, and preview/apply behavior. That historical browser evidence was not repeated in this test audit. Any changed registration UI still needs a fresh browser pass for the changed paths. In particular, a future regression suite should exercise an older delayed response arriving after a newer request, service/address edits while a request is pending, applying a stale or invalid candidate, edit-to-new cancel and confirm, and preservation of the persisted target before and after that transition.

## Expanded onboarding evidence

`TestOnboardingDraftActivationAndFirstReadAcceptance` drives the composed local UI over `httptest`, a config-scoped sidecar across app recreation, a synthetic TLS service, and the registered MCP server over its in-memory transport. It verifies that draft create/update/delete/resume leave settings and the fake credential vault unchanged; check generations advance monotonically and survive restart; policy review and activation echo the latest successful generation; missing, zero, and stale generations cannot activate; and stale credential, draft, configuration, and restart proofs cannot activate. It also verifies policy confirmation, one new target with unrelated settings preserved, replay without a second side effect, and current first-read evidence only after a successful registered service read.

The dedicated checker test `TestOnboardingConcurrentChecksOnlyNewestGenerationCanPersistOrIssue` gates two live synthetic TLS checks for one draft revision, saves the newer generation first, and verifies that the older late result receives no permit and cannot replace the receipt. It also verifies that starting a newer generation invalidates the prior permit. The checker owner reports its race-enabled focused command passed.

Focused verification passed after the generation contract was added:

```text
go test -count=1 -run '^TestOnboardingDraftActivationAndFirstReadAcceptance$' .
ok   local-agent-harness  1.774s

go test -race -count=1 -run '^TestOnboardingDraftActivationAndFirstReadAcceptance$' .
ok   local-agent-harness  3.113s
```

This evidence does not establish browser interaction, real-provider behavior, OS credential-store behavior, named-secret activation, or a real stdio client flow. The cross-wave matrix contains the current evidence boundaries and any remaining verification status.
