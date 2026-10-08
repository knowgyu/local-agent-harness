# Local Agent Harness v0.1.0-rc.2

Windows x64용 사전 공개 후보입니다. 패키지는 서명되지 않았습니다. SHA-256은 다운로드 파일의 동일성과 손상 여부를 대조하지만 게시자 신원을 증명하지는 않습니다.

## 패키지 식별

- 파일: `local-agent-harness-v0.1.0-rc.2-windows-amd64.zip` (9개 파일)
- 내부 `manifest.json`의 `package_version`: `v0.1.0-rc.2`
- SHA-256: `ce674bbb40934e0f5d33b23ea290ec6be3a719bed3efeb1c41d071024112c025`
- 공개 자산 목록: `release-manifest.json` (ZIP 내부 manifest와 별도 파일)
- 공개 자산 체크섬: `SHA256SUMS.txt`

## 주요 기능

한국어를 기본 언어로 하는 로컬 UI에서 사용자는 이름 있는 연결 대상과 허용 작업을 관리합니다. MCP는 등록된 대상과 허용된 작업만 노출하고 요청마다 그 범위를 검사합니다. 모델이 임의 endpoint나 자격 증명을 지정하는 방식은 지원하지 않습니다.

서비스 자격 증명은 Windows Credential Manager에 저장하고 설정에는 참조만 둡니다. 별도의 사용자 환경 변수 기능은 Windows 현재 사용자 범위에 값을 저장합니다. 같은 사용자 계정의 다른 프로그램도 환경 변수 값을 읽을 수 있습니다. Credential Manager의 값은 환경 변수로 복사되지 않습니다.

`/readiness` 화면은 로컬 설정과 확인 상태를 보여주는 안내입니다. 등록 상태, 과거 연결 이력 또는 준비 상태만으로 실제 AI 클라이언트 연결이나 도구 호출 성공을 뜻하지 않습니다.

## 설치와 실행

다운로드 파일과 체크섬을 대조하는 절차, 설치·업데이트·제거 명령은 [Windows 시작 안내](START_HERE.ko.md)에 있습니다. AI 클라이언트의 수동 설정 예시는 ZIP 안의 `CLIENT_SETUP.md`에 있습니다. 공급자별 구문은 바뀔 수 있으므로 그 문서의 공식 링크에서 최신 안내를 확인하세요.

일반 실행은 loopback에서 로컬 UI를 엽니다. `local-agent-harness.exe --mcp`는 AI 클라이언트가 시작하는 MCP stdio 프로세스입니다. UI 주소를 MCP 서버 주소로 입력하는 방식이 아닙니다.

## 검증 범위

이번 소스 검사와 exact ZIP 검사 결과는 [검증 요약](VALIDATION_SUMMARY.ko.md)에 있습니다. ZIP은 PowerShell 7과 Windows PowerShell 5.1에서 제한된 설치 수명주기 사례를 통과했고, 별도 격리 MCP 실행에서 초기화와 도구 목록을 확인했습니다. 실제 회사 서비스·실제 자격 증명·운영 데이터·외부 클라이언트의 실제 도구 호출은 확인하지 않았습니다. 사용자가 별도 비운영 환경에서 확인할 절차는 [사용자 수용 체크리스트](USER_ACCEPTANCE_CHECKLIST.md)에 있습니다.
