# Registration onboarding implementation contract

Updated: 2026-10-12. This document remains the normative shared backend/API contract; it is not a verification report. Focused/race suites, the latest uncached full suite (`go test -count=1 ./...`, 29.452s), `go vet ./...`, native build, and independent backend security review pass. Functional CUA passed all three synthetic-service draft/check/review/activation flows before the cosmetic-only edits. The bounded visual finish review ended SHIP for the two scored desktop fixes only; final top/operation-card captures at natural 1888px width had no overflow or console warnings/errors. It did not rescore the prior registration, policy, and activation states or the rest of the UI. Restored-helper CUA verified that the readiness UI showed three enabled synthetic targets and no observed MCP first reads; no external MCP read or client invocation was performed. The user decided on 2026-10-12 that current UI use and acceptance are desktop-only; mobile is not used and is not a gate. RC3 ZIP/MCPB builds, MCP smoke, and all 60 targeted lifecycle checks pass. The release owner will generate and verify the final public release manifest/checksums after the documentation commit. Actual provider, OS vault, and Desktop/client first-read evidence remain outside current proof. See [the verification matrix](ONBOARDING_VERIFICATION_2026-10-11.md) for evidence status. Product requirements come from [PRODUCT.md](../../PRODUCT.md), [UX research](UX_RESEARCH_2026-10-10.md), and [the decision record](UX_DECISIONS_2026-10-10.md).

The flow is intended for Korean-first use, a balanced setup/operations experience, URL-first GitHub Enterprise/Jenkins/Dashboard registration, guided token and permission setup, and roughly one to five connections. Its completion criterion is an AI client successfully reading from the external service. It must preserve a resumable ordinary draft without treating it as an active target. ChatGPT Desktop or Claude Desktop is the preferred eventual validation path; Codex CLI is the preferred currently supported fallback. Dashboard's displayed 1.7.0 and token-login report do not identify its backend/authentication track.

## State boundaries

There are three distinct records:

| State | Meaning | Existing or planned storage |
| --- | --- | --- |
| Ordinary onboarding draft | User-authored, incomplete, resumable candidate that is not callable by MCP | Versioned sidecar at `<config directory>/onboarding/<sha256(normalized absolute config path)>/connection-onboarding.json`, managed by `onboardingDraftStore` |
| Registered target | Complete service target with an ID and secret reference; it can be active or disabled | Existing strict config v9 and existing target-save paths |
| AI-generated setup draft | Short-lived reviewed change proposal submitted through MCP, potentially covering connections, bundles, SSH, or Python | Existing setup-draft queue/controller |

Ordinary onboarding activation is **create-only** in this wave. Editing an existing registered target continues through the existing manual edit/save path. A URL match must not select, update, or overwrite an existing target. Disabled targets are already registered complete targets; they are not a representation for an incomplete onboarding draft. No fake target, synthetic secret reference, or config v10 migration is part of this contract.

## Shared Go contract

`onboarding_contracts.go` is the shared definition. The sidecar's safe candidate is `onboardingDraftFields` (name and normalized service fields only). It deliberately excludes the original pasted URL, URL query/fragment, credentials, Credential Manager refs, and named-secret IDs; normalized service origin/base URL and other candidate fields are necessary draft data. Incomplete fields are allowed. Ordinary draft CRUD uses a draft ID plus monotonically increasing revision for compare-and-swap; stale writes fail and edits clear the prior successful check.

The shared `onboardingDraftStore` owns a versioned, size-limited, atomically replaced sidecar envelope at `<config directory>/onboarding/<sha256(normalized absolute config path)>/connection-onboarding.json` for ordinary drafts, the latest safe check receipt, activation recovery data, and first-read receipts. Lowercase-normalize the absolute config path on Windows before hashing so separate config files in one directory remain isolated. The subdirectory keeps the active config directory's ACL boundary and leaves config v9 untouched. The store does not read config, resolve secrets, make network requests, or acquire the config lock. Cross-process sidecar locking is required. During activation its lock order is sidecar lock then config lock. It reserves and durably reuses a stable target ID before applying a save, keeps pending work on failure, and removes the ordinary draft only after successful activation. Apply must be idempotent for a retry with the same reserved target ID. Normal paths must never acquire sidecar lock while already holding the config lock.

`WithActivation` supplies the transient `onboardingDraft.ActivationRetry` flag (`json:"-"`) to the activation callback. It is false for a newly created reservation and true whenever a pending reservation already exists. A retry must exact-read the committed target and finalize only when that target matches the reserved activation; if it is absent or does not match, return the ambiguous/manual-recovery result before invoking a service saver or releasing pending state. The durable reservation retry gate remains in force if persisting `RecoveryRequired` fails or the process exits before that marker is written.

`setupDraftConfigRevision(cfg)` is the existing canonical strict-config snapshot digest helper. Carry its exact `config:sha256:<hex>` value independently as `ConfigRevision` on check receipts, policy reviews, activation CAS, and activation records. `CandidateDigest` uses `sha256:<hex>`; fixed `PolicyDigest` uses `policy:sha256:<hex>` and covers only the fixed service policy/review notes, not candidate or configuration state. Candidate, policy, and config digests bind separate things.

`CheckGeneration` is a separate, monotonically increasing non-secret counter on the draft. `StartCheck(ctx, draftID, expectedRevision)` increments it and clears the prior receipt while holding only the sidecar lock, before any credential lookup or network request. Ordinary draft `Revision` does not change. The check receipt, policy-review snapshot, activation CAS, and activation record carry the generation. `SaveCheckReceipt` and activation require the current generation. The process-local proof registry advances the same generation when a check starts and invalidates older permits; a delayed older response cannot restore a receipt or permit after a newer check begins. Neither the store nor registry lock is held during network access.

### Check and activation credential binding

An explicit check is a POST-only, pre-save remote read using the validated draft candidate and either a one-time credential or an explicitly selected existing named secret. It must not call `runConnectionTest` or `recordConnectionTest`, because those write history for registered targets. Check outcomes use fixed result/failure categories; do not expose endpoint response bodies, URLs, raw errors, or secrets. A check cannot save a target or credential.

`onboardingCheckReceipt` is durable, informational state only. It contains draft ID/revision, candidate and fixed-policy digests, config revision, fixed outcome/failure kind, and time. It has no permit, credential, secret reference, URL, or external response. A successful persisted receipt alone **never authorizes activation**.

The in-memory `onboardingCheckProofRegistry` is shared by the check and activation collaborators for one app-controller lifetime. On a successful check it issues a random opaque `check_permit` valid for five minutes; its process-random HMAC binding covers the exact credential bytes, credential source, transient named-secret identity/metadata snapshot when selected, draft ID/revision/check generation, candidate digest, fixed-policy digest, and config revision. It retains no plaintext credential and writes no fingerprint, permit, named-secret ID, or ref to the sidecar or logs. It must compare bindings in constant time where applicable, expire permits, and invalidate permits on draft mutation/deletion or a newer check. Restart loses all permits and requires a fresh check. Activation must receive the same credential source/value again and validate the permit against the current generation, config snapshot, and selected-secret metadata before saving. Different credentials, changed configuration, changed secret metadata, expired/restarted permit, stale draft/generation, or changed policy fail closed and require recheck/review.

The check API returns a transient `onboardingCheckResult{Receipt, Permit}`; its wire representation includes `check_permit`. Only `Receipt` is saved. The activation request echoes `check_permit`, `check_generation`, `config_revision`, expected draft revision, candidate digest, policy digest, explicit policy confirmation, and the same transient credential source. The durable activation record includes draft revision and check generation so completion replay remains CAS-bound. The policy review distinguishes tools directly included by this target from tools that need a service-bundle mapping; `PolicyNotes` explain states such as Jenkins execution remaining guarded by per-call approval. `ExcludedTools` is reserved for permanent exclusions; the UI omits that section when empty instead of implying that all service operations are included. Review text does not create per-tool authorization. Existing per-call policy enforcement remains authoritative.

### Activation preservation

Activation requires all of the following to match the current snapshot under the existing config coordination lock: draft revision, candidate digest, fixed policy digest, config revision, latest successful check receipt, and live check permit bound to the same credential. The user must explicitly confirm the displayed policy review. No URL-derived target is activated before this gate.

After those checks, create one new target through the existing service-specific save semantics, using the sidecar's stable reserved ID and the current credential/selected named secret. Preserve every pre-existing target and its ID/disabled state/secret ref, unrelated named-secret metadata and Credential Manager entries, service bundles, SSH/Python settings, approval policies, and unrelated connection-test history. Do not reuse `configWithSetupDraftState`, which reconstructs whole target lists. Existing config version remains 9. Failed saves clean up only newly-created secrets as the existing save helper allows and leave the onboarding draft recoverable. Existing registered-target edits remain untouched.

### First-read observation

Only a successful external read from a permitted MCP tool can create an observed receipt. Connection checks, catalogs, failed calls, service writes, SSH, and Python are excluded. `onboardingFirstReadReceipt` uses fixed source `mcp_observed`, service, allowed tool, time, and a required digest of the exact safe target definition. It omits target ID/name, URL, credential, resource identifier, and response body. The receipt vocabulary also accepts Harbor reads, while Harbor draft/activation is outside this three-service registration wave. Readiness matches the digest against the current active definition and presents only tool and time. A local UI confirmation stays a separate `user_reported` claim and cannot create an observed receipt.

No receipt or readiness value proves which Desktop/CLI client issued the request or that the client interpreted the result correctly. Real ChatGPT Desktop, Claude Desktop, and Codex CLI first-read proof must be recorded separately from synthetic or local tests.

## Local route expectations

Routes are loopback-only and use the existing local UI's exact host, origin, and CSRF protections. They accept bounded strict JSON, reject unknown/duplicate/trailing fields, and do not perform URL fetching. State-changing requests require POST/PUT/DELETE as appropriate plus a CSRF token and matching single Origin. The current handler work uses this namespace:

| Method and route | Contract |
| --- | --- |
| `GET /onboarding/drafts` | Return summary metadata only (ID, revision, service, timestamps, safe check outcome) |
| `POST /onboarding/drafts` | Create an incomplete draft |
| `GET /onboarding/drafts/{id}` | Explicitly read one normalized, secret-free candidate to resume it |
| `PUT /onboarding/drafts/{id}` | CAS update with expected revision |
| `DELETE /onboarding/drafts/{id}` | CAS delete and invalidate permits |
| `POST /onboarding/drafts/{id}/check` | Explicit temporary check; response carries transient `check_permit` |
| `POST /onboarding/drafts/{id}/policy-review` | Return current policy and config review snapshot |
| `POST /onboarding/drafts/{id}/activate` | Require policy confirmation, all CAS fields, same credential, and live permit |
| `GET /onboarding/read-receipts` | Return safe observed-read receipts only |

Credential fields are transient request values and must be cleared after use. A selected named-secret ID is also transient; the controller resolves it against current config/metadata at check and activation time. Neither is saved in a draft or returned in a response.

## Test and evidence matrix

Each lifecycle stage needs isolated evidence before it is described as implemented:

| Stage | Required automated evidence |
| --- | --- |
| Draft sidecar | CRUD, incomplete values, CAS stale rejection, atomic/cross-process behavior, restart resume, bounded size, secret-free serialization, no MCP catalog/readiness exposure, deletion does not touch active config/secrets |
| Pre-save check | Explicit route only; synthetic HTTP fixtures; no config/history writes; fixed outcome classifications; timeouts and safe redirects; no secret/ref/raw URL/error leakage; temporary credential cleanup; named secret resolved from current metadata |
| Check permit | Successful check only; exact credential/source/draft/candidate/policy/config/secret-metadata/generation binding; altered credential or config fails; mutation or a newer same-revision check invalidates older work; older out-of-order receipt save is rejected; expiry and controller restart require check; durable receipt without permit is insufficient |
| Policy review/activation | Review describes fixed allowed scope; explicit confirmation and all CAS values required; only one new target is saved; failure recovery reuses stable ID; existing targets/IDs/disabled flags/refs/groups/policies/history and Credential Manager entries are preserved |
| First-read receipt | Only successful permitted external read records; disallowed tool, catalog, check, failure and write do not; readiness matches current safe definition digest; receipt excludes IDs/names/URLs/credential/response; user-reported state remains separate |
| Desktop and actual service | Separate manual or controlled environment evidence for each actual service deployment and selected client. Synthetic fixtures cannot satisfy this row. |

Focused tests should be run by the owners of each changed package/fileset, followed by one integration `go test -count=1 ./...`, `go vet ./...`, and native build after integration. Do not report actual Dashboard compatibility or any Desktop/client first read without that environment evidence. The currently verified URL-first UI slice remains separately evidenced in [the registration implementation plan](REGISTRATION_IMPLEMENTATION_PLAN.md).

## Implementation ownership and merge sequence

The work is separable by durable boundary. Merge only after dependent interfaces compile and their tests pass:

1. Shared contract and local URL interpretation (already present in the URL-first slice), independent of A/B selection.
2. Secret-free sidecar store and CRUD/CAS tests; does not require UI choice, network access, or Dashboard auth selection.
3. Pure fixed-policy review model/digests and synthetic pre-save service check; UI-independent. Dashboard endpoint/auth compatibility must remain fail-closed until verified for the actual track.
4. Short-lived check-permit registry and activation CAS/recovery using existing per-service save functions; create-only; preserves current manual edit flows.
5. Loopback routes and UI lifecycle wiring after the backend DTOs stabilize.
6. MCP success-read observer and readiness display, independent of A/B setup screens but dependent on a successful external-tool call in integration tests.
7. Manual actual-service/Desktop validation after supported install paths and a real test environment are available.

Contract/code anchors include [`onboarding_contracts.go`](../../onboarding_contracts.go), [`setup_draft_durable_queue.go`](../../setup_draft_durable_queue.go), [`setup_draft_config_store.go`](../../setup_draft_config_store.go), [`main.go`](../../main.go), [`jenkins.go`](../../jenkins.go), [`dashboard.go`](../../dashboard.go), and [`readiness.go`](../../readiness.go). The ordinary draft, check, activation/recovery, and first-read code is present; use the verification matrix for the latest test and UI evidence, and keep external service/client claims pending until those environments are exercised.
