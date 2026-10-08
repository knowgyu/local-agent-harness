# Windows에서 시작하기

이 안내는 `v0.1.0-rc.2` Windows x64 패키지의 설치와 기본 실행 절차입니다. 패키지는 서명되지 않았습니다. SHA-256은 파일 무결성을 대조하는 값이며 게시자 신원 확인 수단은 아닙니다.

## 1. 다운로드와 압축 해제

릴리스에서 `local-agent-harness-v0.1.0-rc.2-windows-amd64.zip`과 `SHA256SUMS.txt`를 받습니다. 체크섬 파일에서 ZIP 파일명에 해당하는 해시를 아래 ZIP 해시 결과와 대조하세요. 예상 SHA-256은 `ce674bbb40934e0f5d33b23ea290ec6be3a719bed3efeb1c41d071024112c025`입니다. 값이 다르면 실행하지 마세요. ZIP 내부 `manifest.json`은 패키지 버전을 기록하고, 별도 릴리스 자산 `release-manifest.json`은 공개 자산 목록을 설명합니다.

```powershell
Get-FileHash .\local-agent-harness-v0.1.0-rc.2-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS.txt | Select-String 'local-agent-harness-v0.1.0-rc.2-windows-amd64.zip'
```

ZIP을 새 폴더에 전체 압축 해제합니다. 압축 파일 안에서 스크립트를 실행하지 마세요. PowerShell을 압축 해제한 폴더에서 엽니다.

## 2. 설치와 실행

기본 설치 위치는 `%LOCALAPPDATA%\Programs\Local Agent Harness`이며 현재 Windows 사용자 범위에 설치합니다. 관리자 권한은 필요하지 않습니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
```

설치 후 앱을 실행합니다.

```powershell
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

일반 실행은 loopback 주소에서 로컬 UI를 시작합니다. UI의 `/readiness`는 구성과 점검 상태를 안내합니다. 이 화면만으로 AI 클라이언트 연결이나 실제 도구 호출을 확인했다고 볼 수는 없습니다.

다른 설치 위치를 선택하려면 설치 스크립트에 `-InstallRoot`를 지정합니다. 업데이트와 제거에도 같은 설치 위치를 지정해야 합니다.

## 3. AI 클라이언트 연결

MCP 클라이언트는 실행 파일을 stdio 모드로 시작합니다.

```powershell
.\local-agent-harness.exe --mcp
```

보통 클라이언트 설정에서 이 프로세스를 등록하므로 UI의 loopback 주소를 MCP 서버 URL로 입력하지 않습니다. ZIP 안의 `CLIENT_SETUP.md`에서 사용하는 클라이언트의 절차를 확인하세요. 등록된 설정, 활성 세션에서의 서버 표시, `registered_targets` 호출은 서로 다른 확인 단계입니다. 한 단계의 완료를 다음 단계의 증거로 간주하지 마세요.

## 4. 업데이트와 제거

새 버전 ZIP과 체크섬을 확인하고 새 폴더에 압축을 푼 뒤, 앱을 종료합니다. 새 패키지 폴더의 PowerShell에서 다음을 실행합니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Update-LocalAgentHarness.ps1
```

제거하려면 설치 폴더에서 다음을 실행합니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Uninstall-LocalAgentHarness.ps1
```

다른 설치 위치에는 해당 스크립트에 `-InstallRoot`를 지정합니다. 앱 실행 중에는 설치·업데이트가 거부될 수 있습니다. 복구가 필요한 고정 오류가 나오면 설치 폴더의 복구 상태 파일을 직접 수정하거나 지우지 말고, 상태를 보존해 추가 작업을 멈추세요.

## 보안 경계

서비스 비밀값은 Windows Credential Manager에 저장하며 설정 파일에는 참조만 남깁니다. 사용자 환경 변수는 별도 기능으로, 같은 Windows 계정의 프로세스가 값을 읽을 수 있습니다. 두 저장 경로는 서로 다른 목적이며, 비밀값을 환경 변수로 자동 복사하지 않습니다. 화면·MCP·오류·로그에 비밀 원문을 붙여 넣거나 저장하지 마세요.

실제 서비스 연결, 실제 자격 증명 사용, 외부 AI 클라이언트 호환성은 별도 확인이 필요한 항목입니다. RC2 소스와 정확한 다운로드 파일에서 실행한 검증 및 한계는 [검증 요약](VALIDATION_SUMMARY.ko.md)을 확인하세요.
