# Agent-assisted setup

Use this guide with the v0.1.0-rc.6 Windows x64 package. The app and its MCP server are Windows programs; no Go installation or source build is required for the packaged release.

## Install and register a client

The installed MCP server exposes this guide as `lah://guides/agent-setup`. If the host cannot read MCP resources, use the repository copy or the separate `AGENT_SETUP.md` GitHub Release asset. That asset is not inside the portable ZIP. The ZIP contains `CLIENT_SETUP.md` for offline client-registration steps. If the separate guide is all you have, use the official client references below for current command syntax.

Download the Windows x64 ZIP, `release-manifest.json`, and `SHA256SUMS.txt`. Verify the ZIP name, size, and SHA-256 against the release files. Extract it, inspect the included scripts, then run the installer from the extracted folder:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

Use the app's **Clients** page to inspect a registration plan, create a local backup, apply the reviewed change, and verify it. Do not write client configuration yourself. After registration, use the client's supported reload or start a new session; an active session may not reload settings automatically. References: [Codex MCP](https://developers.openai.com/codex/mcp) and [Claude Code MCP](https://code.claude.com/docs/en/mcp).

## Discover and propose

1. Ask the user which exact source paths to inspect. Read only those paths, such as the selected workspace's `.git/config`, one named runbook, or one selected SSH config. Extract candidate values and safe source labels from the chosen material. Strip URL user-info, query, and fragment before displaying or submitting a remote. The built-in runbook preview finds GitHub references only; the SSH preview returns aliases, not complete targets. Never scan the home directory or read `.env`, credential stores, private keys, or unselected files.
2. Call `setup_status` with its default input. It reads saved settings and retained receipt metadata without contacting services, inspecting client configuration, or loading credentials. On first use it may create or secure the protected onboarding namespace directory used for evidence storage; it does not alter saved settings or receipt records. If `configuration_state` is `unavailable` or `configuration_revision` is empty, stop and report unavailable settings instead of inferring an empty inventory or proposing targets; a target's `first_read.state=unavailable` concerns receipt evidence and does not by itself prevent configuration. `inspect_clients` defaults to false. Ask before setting it to true because it inspects local client configuration.
3. Compare target IDs, types, and names, plus service-bundle and environment names. Ask the user to resolve possible matches; do not add duplicates or infer updates. Do not infer permissions, Jenkins jobs, Harbor projects, or Dashboard mappings from an endpoint or version label.
4. Before submitting a target that needs a credential, have the user create or select a named credential in the local UI. The proposal may contain the non-secret credential name, but never a secret value or credential reference. After any settings or credential change, call `setup_status` again.
5. Use the latest MCP `setup_status.configuration_revision` exactly as `submit_setup_draft.base_revision`. It uses the same revision algorithm as draft submission and is computed from the configuration snapshot returned by that status response. Do not use the separate HTTP readiness page's revision. If settings change or submission reports a stale revision, inspect current status and deliberately prepare a fresh proposal.
6. Submit only confirmed fields, a source label and confidence for each candidate, and any required `credential_name`. Submission creates a short-lived proposal but does not change settings. Copy the returned MCP `id` into the local UI's `draft_id` field, and enter the returned `base_revision` and `digest` at `/ssh-settings#setup-draft-review-heading` to open that specific proposal; the page has no automatic proposal list, credential selector, or proposal editor. If approval is blocked, resolve the missing information in the local UI or candidate data, refresh `setup_status`, and submit one complete replacement proposal. Keep the returned values while reviewing; if they are lost, ask the user before creating another draft because the review page cannot find proposals automatically.
7. Add new targets before proposing a bundle that refers to them. Approve the target proposal, call `setup_status` again, and use the returned target IDs and fresh revision in a separate bundle proposal. A bundle can be proposed with targets that already exist, but IDs for new targets are not available to the same submission that creates them. Do not guess IDs or promise a single approval for new targets and their bundle mapping.

## Verify actual reads

After approval, ask the user to confirm the exact targets and reads. Through the connected client, make one safe read for each enabled target: `github_repository`, `jenkins_registered_job`, `harbor_repository_artifacts`, or `dashboard_deployment_status` only after its bundle/environment mapping exists. Catalog calls and connection tests do not count as service reads.

Call `setup_status` again. `targets[].first_read.state=observed` means a successful MCP read receipt matching the current target definition was retained. `not_observed` means no matching receipt is currently retained; it does not prove that no read ever occurred. The store retains the newest 256 distinct target definitions. If the retention limit prevents determining absence, the state is `unavailable`. A Dashboard first read requires its service-bundle environment mapping. `first_read.source=mcp_observed` confirms an MCP read; `client_attribution=not_observed` means the server did not identify which client made it. Do not attribute a read to Codex or Claude unless the client independently proves that identity.

RC6 release evidence is version-specific; consult the separately attached `VALIDATION_SUMMARY.ko.md` for the checks actually completed on RC6. RC5's WSL Codex CLI mock-service call is historical RC5 evidence only. Its Claude Code attempt stopped before a tool call because OAuth had expired, so Claude Code success remains unverified. Do not carry RC5 timings, case counts, or client outcomes forward as RC6 results. Mock-service and protocol-fixture results do not establish real-provider connectivity or client-host acceptance.

For real targets, request explicit confirmation for the exact read and scope before invoking it. Report only evidence the host actually observed. If a client blocks a tool pending approval, report the block and request permission for that exact read. Do not disable approvals globally. An expired client login is not a service-target failure; ask the user to sign in through the client's normal flow. Do not read or copy authentication files.

## Tool annotation boundary

MCP `readOnlyHint` metadata says a tool is intended not to mutate the connected service or configured resources. A successful registered-service read may still persist a bounded receipt in the app's local evidence store. The hint does not authorize a call, expand its scope, or replace normal policy and approval checks. Default `setup_status` does not contact services or load credentials, though first use may create or secure a protected local evidence directory as described above.
