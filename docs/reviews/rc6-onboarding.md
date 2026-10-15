# RC6 onboarding documentation review

## Scope

Reviewed the agent setup workflow against draft submission, review UI, readiness status, and evidence-store behavior. Updated the canonical agent guide, README, client setup note, security boundary, and RC6 release instructions. RC5 release documents remain historical records.

## Findings addressed

- The MCP `setup_status.configuration_revision` now supplies the same canonical saved-configuration revision expected by `submit_setup_draft.base_revision`. The HTTP readiness page has a separately scoped revision and is not a substitute.
- Named credentials must be saved or selected before submission. The user pastes the response's `id` into the UI's `draft_id` field and enters `base_revision` and `digest` at `/ssh-settings#setup-draft-review-heading`; the UI has no automatic proposal list, editor, or credential selector. A blocked proposal must be replaced after missing information is corrected and status is refreshed.
- New target IDs are unavailable to the submission that creates them. The guide now stages target approval first, then reads the assigned IDs and current revision before proposing bundle mappings.
- Default `setup_status` does not contact services, inspect clients, or load credentials. Its first use may create or secure the protected onboarding namespace directory; saved settings and receipt records remain unchanged.

## Evidence boundary

RC5's WSL Codex CLI mock-service call is historical RC5 evidence. The RC5 Claude Code attempt stopped before a tool call because OAuth had expired, so Claude Code success remains unverified. Neither result is RC6 evidence. RC6 platform and package results belong in the separate `VALIDATION_SUMMARY.ko.md`; this review does not claim that tests, packages, Windows client installation, or real services were validated.

This review did not run tests, call clients or services, or inspect credentials.

## Validation by the release worker

Both new regressions failed against RC5 at the intended revision/submission boundary. The fixed Windows-native focused tests passed (6.34s), followed by the full suite (41.32s) and vet (1.46s). The fresh-flow test uses the real MCP and UI handlers with fake credentials and service transport. A separate bounded code/test review found no remaining blocker; that reviewer did not execute tests.

The portable build passed (24.00s), as did pinned MCPB validation/packing/info (26.84s). The packaged native EXE reports the canonical draft revision; its default status created two protected directories without changing data files. The final ZIP passed all 12 lifecycle workflows on Windows PowerShell 5.1.26100.9711 and PowerShell 7.6.7, including the published RC5-to-RC6 upgrade and user-file preservation. Each runtime passed 9 recovery and 3 legacy-journal cases. RC4-named legacy parameters were bound to RC6; this is RC3-to-RC6 compatibility, not an RC4 archive test. An initial runner-layout failure occurred before lifecycle actions; the corrected full rerun passed. See the RC6 validation summary for platform and acceptance limits.
