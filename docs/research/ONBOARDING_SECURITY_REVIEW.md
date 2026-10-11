# Onboarding security review — 2026-10-12

**Disposition: no material security or recovery blocker found in the reviewed backend.** The ambiguous credential-cleanup retry hole found during review is closed by a durable manual-recovery gate and a retry guard that also works if the gate write fails or the process stops before it is written. This is a focused review of the current dirty working tree, not release, native-build, browser, or live-service sign-off.

## Scope and evidence

The review traced draft persistence, check start and receipt generations, process-local permits, activation CAS, create-only config saves, recovery/replay, onboarding routes, and first-read receipts against the [implementation contract](ONBOARDING_IMPLEMENTATION_CONTRACT.md) and [verification matrix](ONBOARDING_VERIFICATION_2026-10-11.md).

Independent commands run from the repository root:

```text
go test -count=1 -run '^(TestOnboarding|TestFirstRead|TestRegistrationURL)' .
PASS

go test -race -count=1 -run '^(TestOnboarding|TestFirstRead|TestRegistrationURL)' .
PASS
```

The focused suites include the adversarial gate-write-failure/restart case, activation and store recovery, route error handling, check-generation races, and first-read observation. The fault fixtures use in-memory secrets; they do not access Windows Credential Manager or real service accounts.

## Recovery issue found and disposition

The original guard detected a save helper that returned a reserved target ID with an error after secret cleanup failed, but the durable `RecoveryRequired` marker was written after that callback. A failure to write the marker—or a process stop before the write—could leave a reusable pending reservation and permit a second secret save.

The current store supplies an in-memory `ActivationRetry` flag to the callback whenever it reuses a persisted pending reservation. On the first attempt, an ambiguous outcome sets the durable manual-recovery marker. On a retry, activation inspects the reserved target under the config lock before any save helper: if the target is absent or does not exactly match the candidate, activation returns the recovery-required error without saving or releasing the reservation. This also protects the case where the durable marker write failed. If the exact target is present after config save but sidecar finalization failed, the normal fresh-check path validates the current proof and target, then completes without another save.

The adversarial test injects config replacement failure after the real save helper stores a credential, makes vault deletion fail, fails the second sidecar write that would persist `RecoveryRequired`, restores the config, and reopens the store. It confirms a fresh check can occur, but the reused reservation will not save again; once the gate is persisted, subsequent check/activation attempts stop before network or callback work. A separate activation test covers config-committed/sidecar-finalization-failed recovery with the stable target ID.

## Other reviewed boundaries

- The versioned sidecar keeps normalized candidate fields and omits raw pasted URLs, credentials, secret references, and permits. Draft mutations use revision CAS. `StartCheck` durably advances the generation and clears the prior receipt before credentials or network access.
- Checks stop redirects, classify failures into fixed categories, and run outside config and sidecar locks. A successful persisted receipt alone does not authorize activation. The process-local permit binds the exact credential source/value, candidate, policy, config snapshot, generation, and selected-secret metadata.
- Activation requires the current receipt, permit, policy confirmation, and full CAS. Create-only service wrappers use a stable reserved target ID and preserve unrelated config v9 state. Completed activation replay remains a read-only historical CAS/current-candidate check; it does not claim fresh credential availability.
- A recovery-required draft exposes only a fixed boolean/error to the local UI. Check, review, and activation routes reject it with `activation_recovery_required`. No secret, secret reference, or permit is included in the durable recovery marker.
- First-read receipts capture a digest from the exact loaded active definition and are recorded after a successful registered MCP read. Readiness filters against current active definitions. This is local observation evidence; it does not establish which Desktop or CLI issued the call.

If the reserved target cannot be proven committed or safely retried, the flow intentionally locks the draft for manual recovery. Real GitHub Enterprise, Jenkins, Dashboard, Desktop, and client behavior still needs separate environment evidence; synthetic fixtures do not establish that compatibility.
