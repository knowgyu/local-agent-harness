# v0.1.0-rc.3 검증 요약

## 판정 범위

RC3의 target-free 코드·synthetic lifecycle 검증은 통과했습니다. 이 문서는 실제 공급자 연결, Windows OS credential vault 저장, 실제 Desktop/CLI 호출 또는 실제 MCP first-read를 통과했다고 주장하지 않습니다. UI 사용·시각 acceptance는 desktop-only입니다. 두 scored cosmetic fix(상단의 redundant eyebrow, lower operation-card side accents)의 bounded visual review는 SHIP 판정을 받았습니다. 정확한 최종-source 캡처는 이 두 영역만 자연스러운 1888px client width에서 확인했으며 scrollWidth도 1888px였습니다. Console warning/error와 visible regression은 없었습니다. 앞선 registration/policy/activation 3-state evidence는 cosmetic-only 변경 전 source의 캡처이며, 최종 bounded review에서 전체 expanded flow를 재점수화하지 않았습니다. 모바일은 사용하지 않으며 현재 릴리스 게이트가 아닙니다.

## 코드 검증

최신 RC3 source에서 다음 검사가 통과했습니다.

| 검사 | 결과 |
| --- | --- |
| `go test -count=1 ./...` | 통과, 29.452초 |
| `go vet ./...` | 통과 |
| Native Windows amd64 executable build | `-trimpath`, VCS metadata disabled 설정으로 통과 |
| Onboarding focused/race suites | Draft CRUD/CAS/restart, pre-save check, generation ordering, activation/recovery, first-read receipt 검증 통과 |
| Dashboard diagnosis MCP receipt regression | `TestDashboardDiagnosisMCPFirstReadReceiptRequiresSuccessfulSubread` focused 및 `-race -count=3` 통과 |

Dashboard diagnosis regression은 MCP call, registered-target binding과 credential lookup, observer, durable receipt sidecar, readiness까지 통합해 검증합니다. All-subreads failure에서는 receipt가 없어 `not_observed`로 남고, partial success에서는 현재 target definition digest에 bound된 receipt 하나가 저장되어 readiness에 표시됩니다. 독립 보안 재검토는 새 테스트에서 material false positive나 binding 약화를 찾지 않았습니다.

Adversarial recovery tests cover config-write plus secret cleanup failure and `RecoveryRequired` marker persistence failure followed by restart. Existing-pending retry exact-reads the reserved committed target before read-only finalize; absent/conflicting targets stop for manual recovery before another save. Synthetic vault/config fixtures prove that the tested retry does not save a second secret. They do not exercise Windows Credential Manager.

## 로컬 UI 검증

Isolated synthetic-helper CUA passed sequential URL-first draft, explicit check, policy review, and explicit activation for GitHub Enterprise, Jenkins, and Dashboard. Dashboard check expiry was shown as expired/revoked; recheck, review, and explicit activation then succeeded. URL-selector preservation, credential/field invalidation, localized history, and post-activation hint behavior also passed.

The restored helper reused the same settings path and onboarding sidecar: 0 ordinary drafts, 0 pending activations, 3 completed activations, and 0 first-read receipts. A prior helper connection refusal came from Go's default 10-minute test timeout ending the listener before its 12-hour review timer; the current launcher uses `-timeout=13h`. The separate AI setup-proposal queue is process-local and is not the ordinary onboarding sidecar.

Readiness CUA at a 1888px desktop window showed three enabled synthetic targets, no saved connection tests, missing Dashboard resource mapping, uninspected clients, and no observed MCP first reads. The readiness page showed no horizontal overflow or browser logs. Earlier registration, policy, and activation CUA/screenshots are prior-source evidence from before cosmetic-only edits. The final bounded review recaptured the top and operation-card regions at natural 1888px client/scroll width; the two scored fixes received SHIP, with no console warnings/errors or visible regression in scope. The read-only prompt is 1044px wide, the post-activation empty hint and stale preview are hidden, and first-read handoff copy does not claim that a read has occurred. Mobile is not used and has no gate. The previous **recapture** result referred to a historic desktop/mobile matrix and is superseded as to the mobile requirement.

## RC3 portable package and lifecycle checks

The Windows amd64 RC3 ZIP build passed with 9 archive members, reproducible executable builds, and no privacy markers. The exact RC3 MCPB passed pinned CLI 2.1.2 `validate`, `pack`, and `info`; its schema is 0.3, version is 0.1.0-rc.3, it contains `manifest.json` and `server/local-agent-harness.exe`, and its embedded executable matches the ZIP executable. Packaging checks establish package structure and parity, not Desktop support.

The exact ZIP executable passed isolated stdio initialize with protocol version `2025-11-25` and `tools/list` with 22 unique tools; it exited 0. This smoke run did not use a real client configuration or write credentials to an OS vault. It does not prove a successful call to a real provider or an observed first read.

Native lifecycle checks passed under both PowerShell 7.6.5 and Windows PowerShell 5.1.26100.9444:

| Check group | Each runtime | Total |
| --- | ---: | ---: |
| Basic lifecycle | 7/7 | 14/14 |
| Link-path/reparse cases | 12/12 | 24/24 |
| Recovery lifecycle | 9/9 | 18/18 |
| Root-creation reparse | 1/1 | 2/2 |
| Root lease | 1/1 | 2/2 |
| **Total** | **30/30** | **60/60** |

For publication, `release-manifest.json` will list the six payload assets. `SHA256SUMS.txt` will list those six assets plus `release-manifest.json` (seven entries); it will not hash itself. The release owner will generate and verify the final manifest and sums after the documentation commit. SHA-256 verifies file bytes, not publisher identity. The ZIP and MCPB are unsigned. Packaging instructions are in [`docs/CLAUDE_DESKTOP.md`](../../CLAUDE_DESKTOP.md).

## 미검증 환경

- Actual GitHub Enterprise, Jenkins, and private Kubernetes Dashboard endpoints, permissions, TLS/proxy behavior, and Dashboard deployment/auth track.
- Actual Windows Credential Manager save, resolve, rotation, cleanup, and delete behavior.
- Any external AI client installation, server/tool visibility, actual client-issued MCP call, or end-to-end first read.
- Claude Desktop MCPB installation/launch and any Desktop app's actual use of Windows credentials.

The first-read observer is implemented and covered by synthetic MCP-to-sidecar-to-readiness tests; no external service MCP read was performed in this validation run. Use the [acceptance checklist](USER_ACCEPTANCE_CHECKLIST.md) to keep local code evidence separate from optional real-environment validation.
