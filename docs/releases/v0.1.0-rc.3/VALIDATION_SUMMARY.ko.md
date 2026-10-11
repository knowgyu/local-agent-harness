# v0.1.0-rc.3 검증 요약

## 판정 범위

RC3의 target-free 코드·synthetic lifecycle 검증은 통과했습니다. 이 문서는 실제 공급자 연결, Windows OS credential vault 저장, 실제 Desktop/CLI 호출 또는 실제 MCP first-read를 통과했다고 주장하지 않습니다. UI 사용·시각 acceptance는 desktop-only입니다. 현재 자연스러운 1888px 데스크톱 너비에서 찍은 최종 등록·정책·활성화 캡처 검토에서는 overflow가 없었고, redundant eyebrow와 lower operation-card side accents 두 cosmetic follow-up을 한 번의 bounded fix/recheck로 처리 중입니다. 모바일은 사용하지 않으며 현재 릴리스 게이트가 아닙니다. 정확한 RC3 ZIP/MCPB 패키지 검사와 release asset checksum/lifecycle 확인은 패키징 단계에서 진행해야 합니다.

## 코드 검증

최신 RC3 source에서 다음 검사가 통과했습니다.

| 검사 | 결과 |
| --- | --- |
| `go test -count=1 ./...` | 통과, 28.936초 |
| `go vet ./...` | 통과 |
| Native Windows amd64 executable build | `-trimpath`, VCS metadata disabled 설정으로 통과 |
| Onboarding focused/race suites | Draft CRUD/CAS/restart, pre-save check, generation ordering, activation/recovery, first-read receipt 검증 통과 |
| Dashboard diagnosis MCP receipt regression | `TestDashboardDiagnosisMCPFirstReadReceiptRequiresSuccessfulSubread` focused 및 `-race -count=3` 통과 |

Dashboard diagnosis regression은 MCP call, registered-target binding과 credential lookup, observer, durable receipt sidecar, readiness까지 통합해 검증합니다. All-subreads failure에서는 receipt가 없어 `not_observed`로 남고, partial success에서는 현재 target definition digest에 bound된 receipt 하나가 저장되어 readiness에 표시됩니다. 독립 보안 재검토는 새 테스트에서 material false positive나 binding 약화를 찾지 않았습니다.

Adversarial recovery tests cover config-write plus secret cleanup failure and `RecoveryRequired` marker persistence failure followed by restart. Existing-pending retry exact-reads the reserved committed target before read-only finalize; absent/conflicting targets stop for manual recovery before another save. Synthetic vault/config fixtures prove that the tested retry does not save a second secret. They do not exercise Windows Credential Manager.

## 로컬 UI 검증

Isolated synthetic-helper CUA passed sequential URL-first draft, explicit check, policy review, and explicit activation for GitHub Enterprise, Jenkins, and Dashboard. Dashboard check expiry was shown as expired/revoked; recheck, review, and explicit activation then succeeded. URL-selector preservation, credential/field invalidation, localized history, and post-activation hint behavior also passed.

The restored helper reused the same settings path and onboarding sidecar: 0 ordinary drafts, 0 pending activations, 3 completed activations, and 0 first-read receipts. A prior helper connection refusal came from Go's default 10-minute test timeout ending the listener before its 12-hour review timer; the current launcher uses `-timeout=13h`. The separate AI setup-proposal queue is process-local and is not the ordinary onboarding sidecar.

Readiness CUA at a 1888px desktop window showed three enabled synthetic targets, no saved connection tests, missing Dashboard resource mapping, uninspected clients, and no observed MCP first reads. The page showed no horizontal overflow or browser logs. Fresh final registration, policy, and activation captures were taken at the natural 1888px desktop client width with no override. The finish review found no overflow and noted the two cosmetic follow-ups above. The read-only prompt is 1044px wide, the post-activation empty hint and stale preview are hidden, and first-read handoff copy does not claim that a read has occurred. Mobile is not used and has no gate. The previous **recapture** result referred to a historic desktop/mobile matrix and is superseded as to the mobile requirement.

## Claude Desktop MCPB boundary

The Claude Desktop extension path is a separate unsigned MCPB preview, not a Windows installer or proof of Desktop support. An earlier local preview package passed pinned MCPB validation/pack/info checks and archive-member/hash-parity inspection. Exact RC3 MCPB and release ZIP validation is pending packaging and must be rerun against the final assets. Packaging instructions are in [`docs/CLAUDE_DESKTOP.md`](../../CLAUDE_DESKTOP.md). No package check shows that Claude Desktop installed or launched the extension, displayed its tools, or called the server. No ChatGPT Desktop or Codex CLI actual-session call was recorded either.

## Release asset verification

When the RC3 assets are produced, use the exact file names in `release-manifest.json` and compare each asset against `SHA256SUMS.txt`; exact package/archive/checksum verification remains pending at this snapshot. SHA-256 verifies the bytes, not the publisher. The code and native build results above do not replace inspection or installation testing of the exact release ZIP. The package is unsigned.

## 미검증 환경

- Actual GitHub Enterprise, Jenkins, and private Kubernetes Dashboard endpoints, permissions, TLS/proxy behavior, and Dashboard deployment/auth track.
- Actual Windows Credential Manager save, resolve, rotation, cleanup, and delete behavior.
- Any external AI client installation, server/tool visibility, actual client-issued MCP call, or end-to-end first read.
- Claude Desktop MCPB installation/launch and any Desktop app's actual use of Windows credentials.

The first-read observer is implemented and covered by synthetic MCP-to-sidecar-to-readiness tests; no external service MCP read was performed in this validation run. Use the [acceptance checklist](USER_ACCEPTANCE_CHECKLIST.md) to keep local code evidence separate from optional real-environment validation.
