# Windows 데스크톱에서 시작하기 · v0.1.0-rc.7

RC7는 Windows x64용 사전 공개 후보입니다. 체크섬은 다운로드한 파일의 무결성을 확인하며 게시자 인증을 뜻하지 않습니다. 실제 자산 이름·크기·SHA-256은 같은 릴리스의 `release-manifest.json`과 `SHA256SUMS.txt`에서 대조하세요.

## 1. 내려받고 압축을 확인합니다

GitHub Releases에서 RC7 Windows x64 ZIP, `release-manifest.json`, `SHA256SUMS.txt`를 받습니다. manifest의 파일명·크기·SHA-256이 ZIP 및 체크섬 파일과 일치하는지 확인한 뒤 새 폴더에 압축을 풉니다. ZIP 안의 스크립트를 실행하기 전에 내용을 살펴보세요.

```powershell
Get-FileHash .\local-agent-harness-v0.1.0-rc.7-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS.txt | Select-String 'local-agent-harness-v0.1.0-rc.7-windows-amd64.zip'
```

## 2. 앱을 설치하고 시작합니다

압축을 푼 폴더에서 PowerShell을 열어 설치 스크립트를 실행합니다. 기본 설치 위치는 현재 사용자의 LocalAppData입니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

브라우저에서 로컬 UI가 열리면 **사용 가이드**를 선택하세요. 실행 파일의 `--mcp` 모드를 클라이언트에 등록할 때는 ZIP의 `CLIENT_SETUP.md`를 참고하세요. `AGENT_SETUP.md`는 ZIP 구성원이 아니며 GitHub Release의 별도 첨부 파일입니다. 설치된 MCP 서버에서도 `lah://guides/agent-setup`으로 같은 안내를 읽을 수 있습니다.

## 3. 에이전트 초안을 단계별로 검토합니다

1. 에이전트가 읽을 자료는 사용자가 지정한 정확한 경로로 제한합니다. `.env`, 자격 증명 저장소, 개인 키, 선택하지 않은 파일을 읽지 않습니다.
2. 필요한 secret은 먼저 로컬 UI에서 저장하거나 기존 named credential을 선택합니다. 에이전트에는 비밀이 아닌 credential 이름만 전달합니다. secret 값과 credential reference는 대화나 초안에 넣지 않습니다.
3. 최신 MCP `setup_status.configuration_revision`을 제안의 `base_revision`에 그대로 사용합니다. 설정 또는 credential을 저장한 뒤에는 `setup_status`를 다시 호출해 최신 revision을 받습니다. HTTP 준비 상태 페이지의 revision을 대신 쓰지 않습니다.
4. `submit_setup_draft` 응답의 `id`를 로컬 UI의 `draft_id` 입력란에 넣고, 응답의 `base_revision`과 `digest`도 `/ssh-settings#setup-draft-review-heading`에 입력해 해당 제안을 엽니다. 이 화면에는 자동 제안 목록이나 credential 선택기, 제안 편집기가 없습니다. 필수 항목 때문에 승인이 막히면 로컬 설정이나 후보 자료를 먼저 보완하고 최신 상태를 확인한 뒤 완성된 대체 제안을 제출합니다. 반환된 참조값을 보관하고, 유실되면 사용자에게 확인하기 전 새 제안을 만들지 않습니다.
5. 새 대상부터 제출하고 승인합니다. 그런 다음 상태를 다시 조회해 새 대상 ID와 revision을 받고, 그 ID를 참조하는 서비스 묶음·환경 매핑을 별도 제안으로 제출합니다. 새 대상과 매핑을 한 번의 승인으로 처리할 수 있다고 가정하지 않습니다.

## 4. 사용자가 허용한 MCP 읽기를 확인합니다

정확한 대상과 읽기 범위를 사용자에게 확인한 뒤에만 실제 MCP 읽기를 실행합니다. 카탈로그 조회와 연결 시험은 실제 서비스 데이터 읽기가 아닙니다. 작업 후 `setup_status`에서 대상별 `first_read.state=observed`를 확인하세요. 이 값은 보관된 성공 MCP 읽기 영수증을 뜻하며 어느 클라이언트가 호출했는지는 증명하지 않습니다. `not_observed`도 과거에 읽지 않았다는 증거가 아닙니다.

기본 `setup_status`는 외부 서비스에 연결하거나 자격 증명을 불러오지 않고 클라이언트 설정도 검사하지 않습니다. 최초 호출 때 보호된 로컬 증거 디렉터리를 만들거나 보호 설정을 보완할 수 있지만, 저장된 설정과 영수증 레코드는 변경하지 않습니다.

## RC7 결과와 한계

실제로 완료한 RC7 테스트와 패키지 확인은 별도 릴리스 첨부 파일 `VALIDATION_SUMMARY.ko.md`에서 확인하세요. RC5의 Codex CLI WSL 모의 호출과 만료된 OAuth로 도구 호출 전에 막힌 Claude Code 시도는 RC5 기록이며 RC7 결과가 아닙니다. 실제 사내 서비스, Windows Credential Manager, 클라이언트 설치 및 서비스 호환성은 해당 요약에 근거가 기록되지 않는 한 검증된 것으로 보지 마세요.
