# Registration UI live QA helper

Start the isolated Windows review helper with:

```powershell
.\scripts\start-registration-ui-live-qa.ps1
```

The launcher runs only `TestRegistrationUILiveQAHelper`, hides the process, waits for its metadata, and prints the loopback URL, helper PID, settings path, and exact stop-file path. It passes `-timeout=13h` to `go test`; Go's default 10-minute test timeout otherwise ends the helper long before the test's 12-hour review limit. The helper also runs an initial local page/form check before it publishes metadata.

To reuse a saved synthetic fixture under `output/ux-preview/runtime`, pass its settings file explicitly:

```powershell
.\scripts\start-registration-ui-live-qa.ps1 `
  -ReuseSettingsPath .\output\ux-preview\runtime\<prior-run>\settings.json
```

Reuse is limited to one enabled GitHub target (`github.qa.invalid`, `synthetic/hello-world`), one enabled Jenkins target (`jenkins.qa.invalid`, `qa-bot`, `qa-smoke`), and one enabled Dashboard target (`dashboard.qa.invalid/proxy`). The helper rejects settings outside the runtime directory or with other targets. It restores only the fixed synthetic fixture credentials in an in-memory store; it does not read Windows Credential Manager or persist credential values. The service transport opens no external socket and permits only the exact synthetic HTTPS hosts and read-only API paths.

Using the same settings path also selects the same path-hashed onboarding sidecar. That sidecar keeps ordinary onboarding drafts, activations, and first-read receipts across helper restarts. The helper's separate `fakeSetupDraftQueueStore` for MCP setup proposals is process-local and starts empty after a restart; it is not the ordinary onboarding sidecar.

Each helper writes its own `metadata.json` and `settings.json` under a unique directory in `output/ux-preview/runtime`. Use the metadata's `stop_file` to end the corresponding helper. The helper exits when that file appears or after 12 hours. Preserve its settings and onboarding sidecar when restarting with `-ReuseSettingsPath`; a no-argument launch creates a new empty synthetic fixture.

The page displays **합성 서비스 검증 화면 · 실제 비밀값 입력 금지**. Use only the printed loopback URL and the listed synthetic fixture. This helper does not install or configure an AI client and does not establish behavior in a production service or desktop app.
