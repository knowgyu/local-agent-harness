# Local Agent Harness

Local Agent Harness는 GitHub, Jenkins, Harbor, Kubernetes Dashboard처럼 이름을 붙인 개발 서비스와 허용된 읽기 작업을 Windows 로컬 UI에서 관리하고, 승인한 범위만 MCP로 제공하는 앱입니다. 외부 서비스 비밀값은 Windows Credential Manager에 저장합니다.

## Windows 설치

[GitHub Releases](https://github.com/knowgyu/local-agent-harness/releases/tag/v0.1.0-rc.5)에서 `v0.1.0-rc.5` Windows x64 ZIP, `release-manifest.json`, `SHA256SUMS.txt`를 받습니다. manifest의 파일명·크기·SHA-256과 ZIP을 대조한 뒤 새 폴더에 압축을 풉니다.

```powershell
Get-FileHash .\local-agent-harness-v0.1.0-rc.5-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS.txt | Select-String 'local-agent-harness-v0.1.0-rc.5-windows-amd64.zip'
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

RC4 기준 소스는 커밋 `80fed06`입니다. 공개 RC4 실행 파일에는 `setup_status`가 없으므로 RC5 설치본을 사용하세요. 설치 후 UI의 **사용 가이드**에서 `/guide`를 열 수 있습니다.

## Codex CLI 또는 Claude Code CLI 에이전트 설정

설치된 CLI 에이전트에 다음처럼 요청하세요.

> 이 저장소의 `docs/AGENT_SETUP.md`를 읽고 Local Agent Harness 설치와 Codex CLI·Claude Code 등록을 진행해 줘. 내가 지정한 자료에서만 연결 후보를 찾고, 빠진 정보는 한 번에 물어봐. 비밀값은 채팅에 쓰지 말고 로컬 UI 입력과 승인까지 안내해 줘. 실제 MCP 조회 결과와 남은 문제까지 확인해 줘.

서버가 이미 연결돼 있다면 `lah://guides/agent-setup` 리소스로 같은 안내를 읽을 수 있습니다. 저장소가 없으면 RC5의 별도 첨부 파일 `AGENT_SETUP.md`를 사용하세요. 안내 파일은 ZIP 외부에 있습니다. 별도 스킬 설치를 전제하지 않습니다. 클라이언트 등록은 UI의 **Clients** 계획·백업·적용·확인 절차를 사용하고, 직접 명령을 쓸 때는 [Codex CLI 설정](docs/CLIENT_SETUP.md#codex-cli) 또는 [Claude Code CLI 설정](docs/CLIENT_SETUP.md#claude-code-cli)을 따르세요. 공식 문서: [Codex MCP](https://developers.openai.com/codex/mcp), [Claude Code MCP](https://code.claude.com/docs/en/mcp).

## 완료 확인과 복구

| 확인 결과 | 의미 |
|---|---|
| `setup_status.schema_version`이 `1` | RC5 설정 상태 도구를 사용할 수 있습니다. |
| `configuration_state` | 저장된 설정 상태입니다. 설치·실제 조회 전체의 성공 판정은 아닙니다. |
| `clients`의 등록/설정 상태 | 클라이언트 설정을 확인했습니다. 실제 도구 호출이나 서비스 읽기를 증명하지는 않습니다. |
| 각 활성 대상의 `targets[].first_read.state=observed` | 해당 대상에서 성공한 MCP 읽기가 기록됐습니다. 카탈로그 조회와 연결 시험은 포함되지 않습니다. |
| `targets[].first_read.source=mcp_observed`, `client_attribution=not_observed` | MCP 읽기는 관찰했지만 호출한 CLI는 식별하지 못했습니다. |
| `targets[].first_read.state=not_observed` | 현재 대상 정의와 일치하는 보관 증거가 없습니다. 과거 읽기가 없었다는 뜻은 아닙니다. |
| `unavailable` | 읽을 수 없거나 보존 한도 때문에 부재를 판단할 수 없습니다. |

Dashboard 읽기는 대상만 등록해서는 부족합니다. 서비스 묶음의 환경에 Dashboard 대상과 namespace/deployment가 연결되어 있어야 합니다. 첫 읽기 이력은 서로 다른 최신 대상 정의 256개까지만 보관합니다. `not_observed`는 현재 정의와 일치하는 보관 증거가 없다는 뜻이지, 과거 읽기가 없었다는 증거는 아닙니다. 보존 한도에 닿아 부재를 판단할 수 없으면 `unavailable`로 표시합니다. 설정이 끝나도 대상별 읽기가 관찰되지 않았다면 미완료로 표시하세요.

도구가 없다면 설치된 바이너리 버전을 확인하고 RC5를 설치한 뒤 클라이언트가 지원하는 reload를 쓰거나 새 세션을 여세요. 초안 흐름이 끊기거나 만료되면 UI에서 기존 제안을 먼저 찾아 이어서 검토하세요. 이전 제안이 없는지 확인하기 전에는 다시 제출하거나 승인을 반복하지 마세요.

## 검증 범위

RC5 검증은 모의 서비스 기준입니다. WSL의 Codex CLI에서 모의 GitHub 읽기와 상태 조회를 실행했고, Claude Code는 OAuth 만료(401)로 실제 호출을 확인하지 못했습니다. 두 클라이언트의 프로토콜 모의 시험은 통과했습니다. 실제 사내 서비스·Windows 자격 증명·클라이언트별 설치 결과는 별도 확인이 필요합니다. 상세 범위는 [RC5 검증 요약](docs/releases/v0.1.0-rc.5/VALIDATION_SUMMARY.ko.md)을 참고하세요.

## 저장소 안내

| 위치 | 내용 |
|---|---|
| 저장소 루트의 Go 파일 | 앱 구현과 테스트. Go 패키지는 현재 구조대로 루트에 둡니다. |
| `docs/` | 현재 제품·사용자·에이전트 안내 |
| `docs/research/` | 설계, 리뷰, 검증 근거 |
| `docs/releases/<version>/` | 버전별로 보관하는 릴리스 안내와 검증 기록 |
| `packaging/` | Windows 패키징과 설치기 구성 |
| `scripts/` | 빌드·개발·릴리스 보조 스크립트 |
| `testdata/` | 자동화 테스트와 모의 환경 fixture |
