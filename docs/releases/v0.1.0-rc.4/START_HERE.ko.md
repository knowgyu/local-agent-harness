# Windows 데스크톱에서 시작하기 · v0.1.0-rc.4

RC4는 Windows x64용 서명되지 않은 사전 공개 후보입니다. 아래 자산 이름과 파일 해시는 실제 배포의 `release-manifest.json` 및 `SHA256SUMS.txt`를 기준으로 확인하세요. 체크섬 일치는 게시자 인증을 뜻하지 않습니다.

## 1. 내려받고 압축을 확인합니다

Releases에서 `local-agent-harness-v0.1.0-rc.4-windows-amd64.zip`과 `release-manifest.json`, `SHA256SUMS.txt`를 받습니다. 목록의 ZIP 이름·크기·SHA-256이 두 파일과 일치하는지 확인한 뒤 새 폴더에 압축을 풉니다. ZIP 안의 스크립트를 실행하기 전에 파일 내용을 살펴보세요.

```powershell
Get-FileHash .\local-agent-harness-v0.1.0-rc.4-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS.txt | Select-String 'local-agent-harness-v0.1.0-rc.4-windows-amd64.zip'
```

일치하지 않거나 manifest에 자산이 없으면 실행하지 마세요. ZIP에는 실행 파일과 설치·업데이트·제거 스크립트, 오프라인용 `USER_GUIDE.ko.md`, `CLIENT_SETUP.md`, `CLAUDE_DESKTOP.md`가 들어 있습니다. 안내서와 클라이언트 문서를 같은 압축 해제 폴더에서 열면 오프라인 상대 링크가 유지됩니다.

## 2. 실행하고 앱 안의 안내를 엽니다

압축을 푼 폴더에서 PowerShell을 열어 현재 사용자 범위로 설치합니다. 관리자 권한은 필요하지 않습니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
```

설치 후 실행 파일을 시작합니다.

```powershell
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

브라우저에서 로컬 UI가 열리면 상단의 **사용 가이드**를 선택하세요. `/guide`는 설정이나 비밀값을 읽지 않고, 원격 서비스에 접속하거나 MCP 도구를 실행하지 않는 정적 안내입니다. 한국어와 영어를 바꿀 수 있으며, URL 등록부터 첫 읽기 확인까지 순서와 예시를 제공합니다. 오프라인에서는 압축을 푼 `USER_GUIDE.ko.md`를 읽으세요.

## 3. 서비스 하나를 등록합니다

안내의 **설정과 운영** 링크를 열고 **연결 대상** 화면에서 GitHub Enterprise, Jenkins 또는 Kubernetes Dashboard를 선택합니다.

1. HTTPS 서비스·저장소·작업 주소를 넣고 **주소 확인**을 선택합니다. 후보 값만 추정하며, 이 단계에서는 원격 서비스에 요청하지 않습니다. 주소에 토큰, 비밀번호, 쿼리 문자열을 넣지 마세요.
2. 후보를 검토하고 필수 값을 채운 다음 일반 설정 진행 상황을 저장하면 나중에 이어갈 수 있습니다. 초안에는 토큰, 비밀번호, 자격 증명 참조가 저장되지 않습니다.
3. 인증 준비 안내에서 해당 서비스의 토큰 발급 위치와 읽기 권한을 관리자 또는 버전에 맞는 공식 문서로 확인합니다. 임시 확인은 **임시 연결 확인 실행**을 눌렀을 때만 제한된 원격 요청을 보냅니다.
4. 확인 후 **고정 작업 범위 검토**에서 읽기 범위를 살펴보고, 직접 확인한 뒤 **이 대상 등록**을 누릅니다. 이 onboarding 흐름은 새 대상만 추가합니다.

가상 URL과 각 서비스의 다음 입력 필드는 오프라인 문서 또는 앱의 **사용 가이드**에 있습니다. 실제 URL이나 비밀값은 공개 문서·AI 요청문·스크린샷에 넣지 마세요.

## 4. 클라이언트에서 실제 읽기를 확인합니다

MCP 서버는 stdio 모드로 시작합니다. UI의 브라우저 주소를 MCP 서버 URL로 입력하지 마세요.

```powershell
.\local-agent-harness.exe --mcp
```

클라이언트별 수동 설정 예시는 ZIP의 `CLIENT_SETUP.md`를 참고하세요. 앱의 대상 목록·서비스 묶음 목록은 로컬 설정 조회입니다. 별도의 등록 대상 연결 테스트는 서비스에 제한된 읽기 요청을 보낼 수 있지만, 어느 쪽도 AI 클라이언트의 첫 외부 읽기 증거는 아닙니다. 준비 상태에서 첫 읽기가 보이려면 연결한 클라이언트가 실제 허용 MCP 도구로 서비스 데이터를 성공적으로 읽어야 합니다.

Codex CLI 설정 안내와 ChatGPT 데스크톱 앱의 Codex 기능 안내는 일반 ChatGPT 대화나 Work 대화의 MCP 연결을 뜻하지 않습니다. 이 저장소는 실제 AI 클라이언트 설치와 호출을 검증하지 않았습니다. Claude Desktop용 `.mcpb`가 배포되더라도 로컬 패키지 검증은 실제 Desktop 설치·실행을 입증하지 않습니다.

## 5. 업데이트와 제거

새 RC 패키지를 별도 폴더에 풀고 앱을 종료한 뒤 제공된 업데이트 스크립트를 실행합니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Update-LocalAgentHarness.ps1
```

제거할 때는 설치 위치에서 제거 스크립트를 실행합니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Uninstall-LocalAgentHarness.ps1
```

설치 위치를 바꿨다면 업데이트와 제거에도 같은 `-InstallRoot`를 지정하세요. RC4는 RC3에서 중단된 설치·업데이트의 인식된 복구 기록을 이어 처리할 수 있습니다. 복구 안내가 나오면 설정과 상태 파일을 직접 지우거나 고치지 말고 안내된 수동 복구 절차를 따르세요.

## 6. 사용 전 경계 확인

이 RC에서는 실제 GitHub Enterprise, Jenkins, Dashboard 서비스 권한·인증·네트워크 연결, Windows Credential Manager의 실저장·삭제, AI 클라이언트 설치·실제 호출·첫 외부 읽기를 확인하지 않았습니다. 합성 테스트와 패키지 검증은 각 환경의 성공을 대신하지 않습니다. 현재 UI 범위는 데스크톱이며 모바일은 사용·수용 범위에 포함되지 않습니다. 자세한 결과는 [검증 요약](VALIDATION_SUMMARY.ko.md)과 [수용 체크리스트](USER_ACCEPTANCE_CHECKLIST.md)를 확인하세요.
