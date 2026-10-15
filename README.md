# Local Agent Harness

Local Agent Harness는 GitHub, Jenkins, Harbor, Kubernetes Dashboard처럼 이름을 붙인 개발 서비스와 허용된 읽기 작업을 Windows 로컬 UI에서 관리하고, 승인한 범위만 MCP로 제공하는 앱입니다. 외부 서비스 비밀값은 Windows Credential Manager에 저장합니다.

## Windows 설치

[GitHub Releases](https://github.com/knowgyu/local-agent-harness/releases/tag/v0.1.0-rc.7)에서 `v0.1.0-rc.7` Windows x64 ZIP, `release-manifest.json`, `SHA256SUMS.txt`를 받습니다. manifest의 파일명·크기·SHA-256과 ZIP을 대조한 뒤 새 폴더에 압축을 풉니다.

```powershell
Get-FileHash .\local-agent-harness-v0.1.0-rc.7-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS.txt | Select-String 'local-agent-harness-v0.1.0-rc.7-windows-amd64.zip'
```

압축을 푼 폴더에서 PowerShell을 열어 설치 스크립트를 실행합니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

RC4 기준 소스는 커밋 `80fed06`입니다. 공개 RC4 실행 파일에는 `setup_status`가 없으므로 RC5 설치본을 사용하세요. 설치 후 UI의 **사용 가이드**에서 `/guide`를 열 수 있습니다.

## Codex CLI 또는 Claude Code CLI 에이전트 설정

설치된 CLI 에이전트에 다음처럼 요청하세요.

> 이 저장소의 `docs/AGENT_SETUP.md`를 읽고 Local Agent Harness 설치와 Codex CLI·Claude Code 등록을 진행해 줘. 내가 지정한 자료에서만 연결 후보를 찾고, 빠진 정보는 한 번에 물어봐. 비밀값은 채팅에 쓰지 말고 필요한 named credential을 먼저 로컬 UI에서 저장한 뒤 이름만 제안에 포함해 줘. 새 대상과 그 대상을 참조하는 서비스 묶음은 단계별로 승인하고 상태를 다시 확인해 줘. 실제 MCP 조회 결과와 남은 문제까지 확인해 줘.

서버가 이미 연결돼 있다면 `lah://guides/agent-setup` 리소스로 같은 안내를 읽을 수 있습니다. 저장소가 없으면 RC7의 별도 첨부 파일 `AGENT_SETUP.md`를 사용하세요. 이 안내 파일은 ZIP 외부에 있고, `CLIENT_SETUP.md`는 ZIP 안에 있습니다. 별도 스킬 설치를 전제하지 않습니다. 클라이언트 등록은 UI의 **Clients** 계획·백업·적용·확인 절차를 사용하고, CLI 명령은 압축에 포함된 `CLIENT_SETUP.md`를 따르세요. 공식 문서: [Codex MCP](https://developers.openai.com/codex/mcp), [Claude Code MCP](https://code.claude.com/docs/en/mcp).

## 완료 확인과 복구

| 확인 결과 | 의미 |
|---|---|
| 최신 MCP `configuration_revision` | 같은 설정 snapshot에 대한 초안 revision입니다. 그대로 `submit_setup_draft.base_revision`에 사용하고, 설정이나 named credential을 저장한 뒤에는 `setup_status`를 다시 호출합니다. |
| `setup_status.schema_version`이 `1` | 도구 스키마 버전입니다. RC7 실행 파일인지 확인하는 용도로 쓰지 마세요. 설치한 실행 파일은 RC7 릴리스 자산과 대조합니다. |
| `configuration_state` | 저장된 설정 상태입니다. 설치·실제 조회 전체의 성공 판정은 아닙니다. |
| `clients`의 등록/설정 상태 | 클라이언트 설정을 확인했습니다. 실제 도구 호출이나 서비스 읽기를 증명하지는 않습니다. |
| 각 활성 대상의 `targets[].first_read.state=observed` | 해당 대상에서 성공한 MCP 읽기가 기록됐습니다. 카탈로그 조회와 연결 시험은 포함되지 않습니다. |
| `targets[].first_read.source=mcp_observed`, `client_attribution=not_observed` | MCP 읽기는 관찰했지만 호출한 CLI는 식별하지 못했습니다. |
| `targets[].first_read.state=not_observed` | 현재 대상 정의와 일치하는 보관 증거가 없습니다. 과거 읽기가 없었다는 뜻은 아닙니다. |
| `unavailable` | 읽을 수 없거나 보존 한도 때문에 부재를 판단할 수 없습니다. |

기본 `setup_status`는 서비스에 접속하거나 자격 증명을 불러오지 않고 클라이언트 설정도 검사하지 않습니다. 최초 호출 때 보호된 로컬 증거 디렉터리를 만들거나 보호 설정을 보완할 수 있지만, 저장된 설정과 영수증 레코드는 변경하지 않습니다.

Dashboard 읽기는 대상만 등록해서는 부족합니다. 서비스 묶음의 환경에 Dashboard 대상과 namespace/deployment가 연결되어 있어야 합니다. 첫 읽기 이력은 서로 다른 최신 대상 정의 256개까지만 보관합니다. `not_observed`는 현재 정의와 일치하는 보관 증거가 없다는 뜻이지, 과거 읽기가 없었다는 증거는 아닙니다. 보존 한도에 닿아 부재를 판단할 수 없으면 `unavailable`로 표시합니다. 설정이 끝나도 대상별 읽기가 관찰되지 않았다면 미완료로 표시하세요.

도구가 없다면 설치된 바이너리를 RC7 릴리스 자산과 대조하고 클라이언트가 지원하는 reload를 쓰거나 새 세션을 여세요. 제안 제출 응답의 `id`를 로컬 UI의 `draft_id` 입력란에 넣고, 응답의 `base_revision`과 `digest`도 `/ssh-settings#setup-draft-review-heading`에 입력해 검토합니다. 이 화면에는 자동 제안 목록이나 credential 선택기, 제안 편집기가 없습니다. 필수 정보가 빠져 승인할 수 없다면 로컬 UI나 후보 정보를 먼저 보완하고, `setup_status`를 새로 호출한 뒤 완성된 대체 제안을 제출하세요. 새 대상이 서비스 묶음에 필요하면 대상부터 승인하고, 새 ID와 최신 revision을 받은 뒤 매핑 제안을 제출하세요. 반환된 참조값을 보관하고, 유실됐다면 사용자에게 확인하기 전 새 제안을 만들지 마세요.

## 회사 환경에서 사용하기

GitHub와 Jenkins 주소는 대상별 설정입니다. 일반 github.com은 공식 API 호스트를, 사내 GitHub Enterprise는 등록한 호스트·포트의 `/api/v3`를 사용합니다. Jenkins는 등록한 기본 주소의 하위 경로와 공백·한글 작업명을 보존합니다. 서비스 요청에는 30초 제한을 적용합니다. VPN·프록시·인증서·SSO 확인과 실패 후 재시도 순서는 [사용 안내](docs/USER_GUIDE.ko.md#회사-환경의-주소와-연결-조건)를 참고하세요.

## 검증 범위

RC7의 현재 검증 상태와 실제 완료 범위는 [RC7 검증 요약](docs/releases/v0.1.0-rc.7/VALIDATION_SUMMARY.ko.md)에서 확인하세요. RC5의 WSL Codex CLI 모의 서비스 호출과 만료된 OAuth로 중단된 Claude Code 시도는 RC5에만 해당하는 과거 기록이며 RC7 검증 결과가 아닙니다. 실제 사내 서비스·Windows 자격 증명·클라이언트 설치 결과는 별도 확인이 필요합니다.

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
