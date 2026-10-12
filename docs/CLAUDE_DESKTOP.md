# Claude Desktop on Windows

Local Agent Harness can run as a local stdio MCP server from Claude Desktop through a Windows-only `.mcpb` desktop extension. The extension packages the Windows executable and a manifest; it does not contain a Local Agent Harness settings file, Credential Manager data, or a user-selected config path.

## Build the local package

The build requires a Windows amd64 executable produced with Go's `-trimpath` build setting, the Go tool (the script checks the executable's build metadata), and the official MCPB CLI. Node.js/npm are needed only when the pinned MCPB CLI is not already installed globally. The script requires the executable path explicitly, validates the manifest, packages it, checks the resulting archive members and executable hash, and writes the package below the ignored `output/claude-desktop` directory.

```powershell
npm install -g @anthropic-ai/mcpb@2.1.2
.\scripts\build-claude-desktop-extension.ps1 `
  -ExecutablePath .\path\to\local-agent-harness.exe `
  -Version 0.1.0-rc.4
```

The default package version is `0.1.0-rc.4`. The builder pins MCPB CLI `2.1.2` and checks the installed CLI version. Use the exact Windows executable whose build and source have been reviewed. The output file is `output/claude-desktop/local-agent-harness-0.1.0-rc.4-windows-amd64.mcpb`. The package is unsigned; the script does not install it or change Claude Desktop settings.

Build the RC.4 package from the exact reviewed Windows release executable. The script verifies the MCPB manifest, the two expected archive members, and byte-for-byte executable hash parity. These checks establish package structure and bytes only; they do not establish Claude Desktop installation, launch, tool visibility, or an MCP call.

The manifest uses MCPB binary-server support and launches `server/local-agent-harness` with `--mcp`. MCPB hosts append `.exe` to a binary command on Windows. The package contains only `manifest.json` and `server/local-agent-harness.exe`. This repository currently has no `LICENSE`, `COPYING`, or `NOTICE` file, so the manifest does not claim a license or include an invented license file. Resolve licensing metadata before any wider distribution.

The MCPB manifest specification marks `privacy_policies` URLs as required when an extension connects to external services that process user data. This app can call user-configured service hosts, and the repository does not currently provide one product privacy-policy URL that accurately covers them. The local preview therefore omits that field and is not ready for directory listing or broader distribution; add an accurate disclosure before publishing. CLI schema validation alone does not establish that Anthropic will accept this package for directory listing or broader distribution.

## Install in Claude Desktop

After reviewing the package and its source executable:

1. Open **Settings → Extensions** in Claude Desktop.
2. Open **Advanced settings** and locate **Extension Developer**.
3. Select **Install Extension…** and choose the `.mcpb` file.
4. Follow Claude Desktop's prompts. If the extension is blocked, check the organization policy or local extension policy with the administrator.
5. In a chat, open **+ → Connectors** and confirm Local Agent Harness and its tools appear. For startup errors, check the Desktop app's Developer settings and extension logs.

These are Anthropic's published steps for privately installing a custom desktop extension. Claude Desktop's remote custom connectors are a different integration path and do not launch this local executable.

## Settings, credentials, and scope

The extension starts the executable under the same Windows account as Claude Desktop. In MCP mode, the app reads the normal Local Agent Harness settings location from `os.UserConfigDir()`—on Windows, `%AppData%\LocalAgentHarness\config.json`—and uses that account's Windows Credential Manager for secrets. The package does not select another profile or provide an override: the executable has no `--config` argument. Configure Local Agent Harness through its normal local UI as the same Windows user before expecting service targets to be available. The launcher and config behavior are implemented in `main.go`; the Windows secret store is implemented in `credential_windows.go`. These implementation files are available in the source repository or source archive.

The manifest lists the MCP tool names, but configured targets and saved action policies determine what those tools can do. A listed tool is not proof that a target is configured, that a credential exists, or that a service call succeeds. Some tools can contact a saved service, run a saved Python task, trigger a saved Jenkins job, or execute a saved SSH operation subject to the application's stored rules and confirmation policy. Review the target and operation scope before enabling or invoking such tools.

For an initial local check, `registered_targets` and `registered_service_bundles` read the saved catalogs and do not contact services. `registered_target_connection_test` does contact the selected saved target and records the result locally; use it only when that specific external request is intended. A successful catalog call or connection test does not establish Kubernetes Dashboard token/authentication behavior beyond the exact request that was made.

## Evidence boundary

The build script's checks can establish that the MCPB CLI accepted the manifest, packed the two expected members, and preserved the supplied executable byte-for-byte. They do not establish that Claude Desktop installed or launched the extension, displayed its tools, made an MCP call, or completed a first-read against an external target. Those results require observation in the actual Claude Desktop installation and, for an external read, a deliberately selected configured target.

## Official references

- Anthropic, [Getting Started with Local MCP Servers on Claude Desktop](https://support.claude.com/en/articles/10949351-getting-started-with-local-mcp-servers-on-claude-desktop) (checked 2026-10-11): local extensions, `.mcpb` installation, Windows permissions and policies, tool visibility, and logs.
- MCPB project, [Manifest specification](https://github.com/modelcontextprotocol/mcpb/blob/main/MANIFEST.md): manifest version `0.3`, binary entry points, Windows `.exe` handling, supported platforms, and tool declarations.
- MCPB project, [CLI documentation](https://github.com/modelcontextprotocol/mcpb/blob/main/CLI.md): `mcpb validate`, `mcpb pack`, and `mcpb info`.

The manifest tool list mirrors the production registrations in `main.go` and `setup_draft_mcp.go`. The repository's `client_claude.go` adapter targets Claude Code CLI configuration (`~/.claude.json`). It is not the Claude Desktop extension integration described here. These implementation files are available in the source repository or source archive.
