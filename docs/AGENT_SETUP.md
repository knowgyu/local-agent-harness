# Agent-assisted setup

Use this guide with the published v0.1.0-rc.5 Windows x64 package. The pinned RC4 source baseline is commit `80fed06`; published RC4 executables predate `setup_status`. No Go installation or source build is required for the published package.

## Read and install

The installed MCP server exposes this guide as the embedded resource `lah://guides/agent-setup`; read it once when the host supports resources. If it does not, use `docs/AGENT_SETUP.md` from a cloned repository or download `AGENT_SETUP.md` as a separate GitHub Release asset beside the ZIP. The standalone asset is not inside the installer or portable ZIP. No skill installation is needed.

Download the RC5 Windows x64 ZIP with `release-manifest.json` and `SHA256SUMS.txt`, verify the ZIP's listed name, size, and SHA-256, then extract it. Start the included installer from PowerShell:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

For Codex CLI or Claude Code CLI, use the app's **Clients** page to inspect the registration plan, create a local backup, apply the reviewed change, and verify it. Command examples are in [Codex CLI setup](CLIENT_SETUP.md#codex-cli) and [Claude Code CLI setup](CLIENT_SETUP.md#claude-code-cli); official references are [Codex MCP](https://developers.openai.com/codex/mcp) and [Claude Code MCP](https://code.claude.com/docs/en/mcp). If this is the flat release asset and its sibling client guide is unavailable, use those official references for current command syntax. Use supported reload or start a new session; do not assume the active session automatically reloads settings. Do not write client configuration yourself.

## Discover and propose

Ask the user which exact source paths to inspect. Read only those paths, such as the selected workspace's `.git/config`, one named runbook, or one selected SSH config. Extract safe candidate fields and source labels. Strip URL user-info, query, and fragment before displaying or submitting a remote. The built-in runbook preview identifies GitHub references only; the SSH config preview returns aliases, not a complete target. Never scan the home directory or read `.env`, credential stores, private keys, or unselected files. Use the existing UI previews when available.

Call `setup_status` with its default input first. `configuration_state` describes saved configuration, not end-to-end success. Compare `configuration_revision`, `observed_at`, and each target's `first_read` evidence. It returns bounded readiness data with `schema_version: 1` without inspecting clients, contacting services, or loading credentials. `inspect_clients` defaults to false; ask before setting it to true because it inspects local client configuration. Compare existing target type/name and service-bundle/environment names before adding anything. If an enabled or disabled target may match, ask the user to resolve it; do not add a duplicate or infer an update. Do not infer service permissions, Jenkins jobs, Harbor projects, or Dashboard mappings from an endpoint or version label.

Ask for all missing non-secret facts together: target names, endpoints, repository/job/project, required account name, environment, and—when Dashboard is involved—the intended bundle/environment/namespace/deployment mapping. The user may select an existing named secret in the local UI or enter a new value there. Never request, inspect, include in a draft, or return secret values or credential references.

Submit one batch through `submit_setup_draft` using `configuration_revision` as `base_revision`, with safe source labels/confidence and only confirmed candidates. Submission creates a short-lived review proposal and does not change settings. The user must review the whole proposal in the local UI, choose or enter credentials there, and explicitly approve it. If interrupted or expired, inspect the UI's existing review state and resume there. Do not resubmit or approve repeatedly without checking whether a proposal already exists.

## Verify actual reads

After approval and the user's confirmation of the exact targets, make one safe read for each enabled target through the connected client: `github_repository`, `jenkins_registered_job`, `harbor_repository_artifacts`, or—only after a bundle/environment mapping exists—`dashboard_deployment_status`. Catalog/list calls and connection tests do not count.

Check `setup_status` again. For each target, `first_read.state=observed` means a successful MCP read receipt matching the current target definition was retained. `not_observed` means no matching receipt is currently retained; it does not prove that no read ever happened. The history retains the newest 256 distinct target definitions. If the retention limit prevents determining absence, the state is `unavailable`; it also means evidence could not be read. A Dashboard first read requires the configured service-bundle environment mapping. Receipt source `mcp_observed` proves that an MCP read occurred; `client_attribution` remains `not_observed`. Do not claim Codex or Claude made the read unless the client independently proves that identity.

RC5 validation uses mock services only. This guide does not claim actual provider connectivity or Codex/Claude client compatibility. For user-selected real targets, get confirmation for the exact read and scope before invoking it, and report only evidence the host actually observed.

If a client blocks a tool pending approval, report the block and request permission for that exact read. Do not disable approvals globally. An expired client login is not a service-target failure; stop and request the client’s normal sign-in, without reading or copying authentication files.

## Tool annotation boundary

MCP `readOnlyHint` metadata indicates that a tool is intended not to mutate the connected service or configured resources. A successful registered-service read may still persist a bounded receipt in this app's local evidence store. The hint is advisory: it does not authorize a call, expand its scope, or replace normal policy and approval checks. `setup_status` with its default input remains passive; it reads saved settings and retained receipt metadata without network access or local writes.
