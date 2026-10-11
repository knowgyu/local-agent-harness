# Windows 데스크톱에서 시작하기 · v0.1.0-rc.3

이 안내는 Windows x64용 사전 공개 후보를 설치하고 local UI와 MCP 흐름을 확인하기 위한 것입니다. UI acceptance 범위는 데스크톱 창입니다. 모바일은 사용하지 않으며 모바일 브라우저를 시험할 필요가 없습니다. 좁은 창도 데스크톱 창으로 취급합니다.

## 1. 다운로드와 파일 확인

Releases에서 Windows x64 ZIP, `SHA256SUMS.txt`, `release-manifest.json`을 받습니다. 자산 이름과 ZIP의 SHA-256을 manifest/checksum에서 확인합니다. 서명되지 않은 빌드이므로 checksum은 파일 무결성을 확인할 뿐 게시자를 인증하지 않습니다.

```powershell
Get-FileHash .\local-agent-harness-v0.1.0-rc.3-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS.txt | Select-String 'local-agent-harness-v0.1.0-rc.3-windows-amd64.zip'
```

두 값이 일치하지 않거나 ZIP 이름이 `release-manifest.json`에 없다면 실행하지 마세요. ZIP을 새 폴더에 전체 압축 해제한 다음 PowerShell을 그 폴더에서 엽니다. 압축 파일 안에서 스크립트를 실행하지 않습니다.

## 2. 설치와 첫 실행

기본 설치 위치는 현재 사용자 범위의 `%LOCALAPPDATA%\Programs\Local Agent Harness`입니다. 관리자 권한은 필요하지 않습니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1
```

설치 후 앱을 실행합니다.

```powershell
Set-Location "$env:LOCALAPPDATA\Programs\Local Agent Harness"
.\local-agent-harness.exe
```

일반 실행은 loopback에서 local UI를 엽니다. 이 주소는 브라우저 UI 용도입니다. 외부 웹 서비스가 아니고 MCP endpoint도 아닙니다.

## 3. 새 연결 등록

Connections의 onboarding에서 GitHub Enterprise, Jenkins 또는 Kubernetes Dashboard를 고르고 HTTPS URL을 확인합니다. Parser는 입력 형식에서 후보 필드만 추출하며 URL 접속, 실제 서비스 확인, 인증 방식 자동 추론은 하지 않습니다.

1. 후보를 검토하고 이름·서비스 필드를 채운 뒤 일반 draft로 저장합니다. draft는 미완성 상태로 남겨 두었다가 다시 열 수 있습니다.
2. 저장 전 검사는 사용자가 **Check**를 눌렀을 때만 요청을 보냅니다. Check 성공은 target 저장이나 연결 이력 기록이 아닙니다.
3. 표시된 읽기 범위와 mapping-dependent capabilities를 검토합니다. 명시적으로 확인한 뒤에만 새 target 하나를 활성화합니다. 이 흐름은 기존 target을 선택하거나 덮어쓰지 않습니다.
4. 활성화 후 saved-target connection test, MCP catalog 조회, 실제 외부 MCP read는 각각 다른 확인입니다. 첫 MCP read는 사용자가 선택한 클라이언트가 실제 허용 읽기를 성공적으로 호출한 경우에만 근거가 됩니다.

직접 입력한 비밀값은 Check와 그 검사에 바인딩된 activation 요청에서만 일시적으로 전달됩니다. draft에는 저장되지 않으며, 성공한 activation은 일반 target용 credential을 저장할 수 있습니다. Existing named secret을 선택해도 draft에 secret ID나 reference를 저장하지 않으므로 draft를 다시 연 뒤 재선택하고 새로 확인해야 합니다. 실제 Windows Credential Manager 동작은 이 RC의 synthetic 테스트 범위 밖입니다.

## 4. MCP 클라이언트 설정과 첫 호출

클라이언트는 stdio 모드로 앱을 시작합니다. UI 주소를 서버 URL로 입력하지 않습니다.

```powershell
.\local-agent-harness.exe --mcp
```

클라이언트별 사용자 범위 설정 예시는 ZIP의 `CLIENT_SETUP.md`에 있습니다. 설정 등록만으로 연결이 동작하거나 도구가 표시된다고 볼 수 없습니다. 사용 중인 클라이언트에서 서버와 tools가 보이는지 확인하고, `registered_targets` 같은 catalog 호출은 로컬 목록 확인일 뿐 외부 서비스 read가 아니라는 점을 구분합니다.

실제 MCP first-read를 수용하려면 비운영 target과 허용된 읽기를 사용하고, 선택한 클라이언트에서 외부 서비스 데이터를 반환하는 성공 호출을 직접 확인합니다. `/readiness`의 `observed MCP first reads`는 실제 성공한 MCP read 관측만 표시합니다. 현재 release 검증은 synthetic fixtures를 사용했으며 실제 service/client read를 수행하지 않았습니다.

## 5. 업데이트와 제거

새 RC3 패키지와 checksum을 확인하고 앱을 종료한 다음, 새로 푼 폴더에서 실행합니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Update-LocalAgentHarness.ps1
```

제거하려면 설치 폴더에서 실행합니다.

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Uninstall-LocalAgentHarness.ps1
```

다른 설치 경로를 선택했다면 업데이트와 제거에도 같은 `-InstallRoot`를 지정하세요. 앱이 실행 중이면 설치 작업이 거부될 수 있습니다. Activation recovery에서 수동 복구가 요구되면 config/sidecar 상태 파일을 직접 지우거나 고치지 말고 상태를 보존한 채 멈춥니다.

## 6. 보안과 증거 경계

RC3 패키지는 서명되지 않았습니다. Windows 서비스 secret은 앱 설정에서 credential reference를 사용하도록 설계됐지만 실제 OS vault 저장·삭제는 이 RC 검증에서 확인하지 않았습니다. 실제 GitHub Enterprise, Jenkins, Dashboard deployment/auth track, client Desktop 설치와 도구 호출도 미검증입니다. Claude Desktop용 `.mcpb`는 별도 unsigned preview build이며, 해당 패키지를 검증했다는 사실은 Claude Desktop 설치나 호출 증거가 아닙니다.

수용 항목과 현재 테스트 근거는 [검증 요약](VALIDATION_SUMMARY.ko.md)과 [사용자 수용 체크리스트](USER_ACCEPTANCE_CHECKLIST.md)를 확인하세요. 실제 비밀값, 사내 주소, 고객 데이터, 개인 경로를 issue나 release 문서에 기록하지 마세요.
