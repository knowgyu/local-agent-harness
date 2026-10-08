# v0.1.0-rc.2 검증 요약

## 패키지 식별

- ZIP: `local-agent-harness-v0.1.0-rc.2-windows-amd64.zip` (9개 파일)
- 내부 `manifest.json`의 `package_version`: `v0.1.0-rc.2`
- ZIP SHA-256: `ce674bbb40934e0f5d33b23ea290ec6be3a719bed3efeb1c41d071024112c025`
- 서명: 서명되지 않음

해당 ZIP에서 압축 내용, 내부 `manifest.json`, 체크섬을 대조했습니다. 외부 자산 `release-manifest.json`은 공개 자산 목록이며 ZIP 내부 manifest와 다른 파일입니다. `SHA256SUMS.txt`에는 공개 자산 체크섬이 있습니다. SHA-256은 파일 동일성과 손상 여부를 확인하지만 게시자 신원을 증명하지 않습니다.

## Windows 코드 검증

Windows 11 x64 (빌드 26200), Go 1.26.7 `windows/amd64`에서 다음 검사가 통과했습니다. 전체 Go 검사는 실제 Windows에서 실행했으며 교차 빌드가 아닙니다.

| 명령 | 결과 |
|---|---|
| `go test -count=1 -timeout 180s ./...` | 통과, 25.759초 |
| `go test -race -count=1 -timeout 180s ./...` | 통과, 68.927초 |
| `go vet ./...` | 통과, 0.608초 |
| `go build -trimpath ./...` | 통과, 0.478초 |

## ZIP 설치 수명주기 수용

아래 검사는 모두 위 SHA-256의 ZIP에서 수행했습니다.

| Windows 환경 | PowerShell | 일반 수명주기 | 링크 경로 검사 | 추가 링크 경로 검사 | 복구 |
|---|---|---:|---:|---:|---:|
| Windows 11 Home, 빌드 26200 x64 | PowerShell Core 7.6.5 | 7/7 | 12/12 | 1/1 | 9/9 |
| Windows 빌드 26200 x64 | Windows PowerShell 5.1.26100.9444 | 7/7 | 12/12 | 1/1 | 9/9 |

각 런타임에서 29/29 사례, 두 런타임 합계 58/58 사례가 통과했습니다. 링크 경로 검사는 PowerShell Core 7에서 심볼릭 링크, Windows PowerShell 5.1에서 디렉터리 접합점을 사용했습니다. 복구 검사는 지정된 시점에 시험용 자식 프로세스를 종료한 뒤 다음 실행에서 상태 복구를 확인한 범위입니다. 전원 손실, 운영 체제 재시작이나 파일시스템 손상 시험은 아닙니다. 설치 수명주기 검사에서는 패키지 실행 파일을 실행하지 않았습니다.

별도의 격리 실행 확인에서 패키지 실행 파일을 `--mcp`로 시작해 MCP 초기화와 `tools/list` 응답을 확인했습니다. 22개 도구 정의가 반환됐습니다. 이는 외부 AI 클라이언트 연결이나 실제 도구 호출을 확인한 결과가 아닙니다.

## 미검증 범위

실제 회사 서비스·인증·자격 증명·고객 데이터, 실제 Dashboard 설치 버전과 경로, 외부 AI 클라이언트의 설치·연결·실제 도구 호출은 확인하지 않았습니다. Windows Credential Manager, 현재 사용자 환경 변수, 로그인 시작 설정과 운영 체제 재시작도 변경하거나 검증하지 않았습니다. 이 항목을 비운영 환경에서 확인하지 않았다면 [사용자 수용 체크리스트](USER_ACCEPTANCE_CHECKLIST.md)에 미확인으로 남기세요.
