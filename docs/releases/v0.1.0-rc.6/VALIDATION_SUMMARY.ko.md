# v0.1.0-rc.6 검증 요약

RC6의 최종 소스와 배포물을 대상으로 실행한 검증입니다. RC5 실행 결과를 RC6 통과 결과로 재사용하지 않습니다. 모의 서비스와 코드 수준의 UI 요청 검증을 실제 클라이언트·회사 환경의 수용 시험과 구분합니다.

## 수정 전후 확인

| 검사 | 결과 |
|---|---|
| 새 설정 버전 회귀 검사 + RC5 원본 | 의도한 버전 불일치로 실패 |
| 새 초기 설정 통합 검사 + RC5 원본 | MCP 등록 제안 제출 단계에서 실패 |
| 수정본 집중 검사 | Windows 네이티브에서 통과, 6.34초; MCP/HTTP 버전 범위와 기존 HTTP 이력 제외 규칙 포함 |
| 전체 테스트 | Windows Go 1.26.7 windows/amd64, `go test -count=1 ./...` 통과, 41.32초 |
| 정적 검사 | 같은 네이티브 환경의 `go vet ./...` 통과, 1.46초 |

새 `TestFreshSetupFlowApprovesAndObservesRegisteredGitHubRead`는 대상이 없는 설정 파일에서 시작합니다. 메모리 자격 증명 저장소를 연결한 실제 비밀값 UI 핸들러로 이름 있는 자격 증명을 저장한 뒤, 다음 경로를 실행합니다.

1. 실제 MCP 세션에서 `setup_status`를 호출하고 반환된 `configuration_revision`을 그대로 등록 제안에 사용합니다.
2. `submit_setup_draft`가 반환한 `id`를 UI의 `draft_id`에 전달하고 `base_revision`·`digest`와 함께 검토·승인 핸들러를 호출합니다.
3. 실제 설정 파일에 대상이 저장됐는지 확인하고, 다시 조회한 ID와 최신 버전으로 묶음 제안을 제출·승인합니다.
4. 등록된 대상을 읽는 `github_repository`가 지정된 모의 HTTP 요청을 정확히 한 번 보내고 저장된 자격 증명을 사용하는지 확인합니다.
5. 파일 기반 읽기 기록이 `observed`·`mcp_observed`로 나타나고, 연결 시험은 `not_tested`로 남는지 확인합니다. 설정·검토·상태 조회가 서비스 요청이나 자격 증명 로드를 유발하지 않는지도 검사합니다.

MCP는 SDK 메모리 전송, UI는 `httptest` 요청, 자격 증명은 메모리 저장소, GitHub는 `.invalid` 주소를 처리하는 모의 전송을 사용했습니다. 실제 브라우저 조작·Credential Manager·CLI 등록·회사 서비스 호출은 아닙니다. 구현 담당자의 Linux 집중 검사도 통과했지만 위 Windows 네이티브 검증을 대신하지 않습니다.

## 최종 배포물

| 검사 | 결과 |
|---|---|
| Windows x64 ZIP | 빌드 통과, 24.00초; 평면 구성원 11개, 두 번 만든 EXE 바이트 일치, 개인정보 표식 검사 통과 |
| 오프라인 문서 링크 | ZIP 안 문서의 상대 파일 링크가 패키지 안에서 해석되는지 확인 |
| Claude Desktop MCPB | 고정된 `@anthropic-ai/mcpb` 2.1.2의 validate·pack·info 통과, 26.84초; manifest와 서버 EXE만 포함, ZIP의 EXE와 바이트 일치 |
| 최종 EXE 네이티브 stdio | 프로토콜 `2025-11-25`, 고유 도구 23개, MCPB 목록과 일치, 원본과 동일한 안내 리소스, 잘못된 인수 거부 |
| 기본 상태 조회 | `config:sha256:…` 형식의 등록용 버전 확인; 격리한 Windows 프로필에서 데이터 파일 변경 없이 보호된 증거 디렉터리 2개 생성 |
| 소스 일치 | 런타임 입력 97개가 현재 소스와 해시·크기 일치; 테스트한 Go 파일과 패키지에 복사한 문서도 원본과 바이트 일치 |
| 설치·업데이트·중단 복구 | Windows PowerShell 5.1.26100.9711 및 PowerShell 7.6.7에서 12/12 작업 통과; 최종 ZIP 설치·업데이트·제거, RC5 → RC6 업그레이드, 사용자 파일 보존 포함 |

두 PowerShell 환경에서 각각 reparse 경로 거부, 루트·직속 부모 디렉터리 잠금, 중단 복구 9건, 이전 저널 호환성 3건, 최종 RC6 설치·업데이트·제거, 공개 RC5에서 RC6로의 업그레이드를 확인했습니다. 이전 저널 검사의 RC4 이름 매개변수에는 해시를 고정한 RC6 ZIP을 전달했습니다. 이는 RC3 → RC6 모의 중단 저널 호환성 검사이며 실제 RC4 배포물 검사가 아닙니다.

최초 실행은 테스트 복사 위치 때문에 이전 저널 검사 사전 확인에서 중단됐습니다. 제품 동작 전에 발생한 검증 실행기 문제로, 원래 소스 경로에서 호출하도록 수정한 뒤 전체 12개 작업을 다시 실행해 통과했습니다. 이 실패를 제품 검사 통과로 합산하지 않았습니다.

배포 실행 파일은 `-mod=readonly -trimpath -buildvcs=false -ldflags="-s -w"`로 빌드했습니다. 개인정보 검사 규칙은 완화하지 않았습니다. 패키지는 서명되지 않았습니다. 공개 파일의 이름·크기·SHA-256은 `release-manifest.json`과 `SHA256SUMS.txt`를 기준으로 확인하세요.

## 검증하지 않은 범위

RC6에서 실제 Codex CLI·Claude Code·Desktop 앱의 등록 및 도구 호출, Windows Credential Manager의 실제 저장·삭제, 실제 GitHub Enterprise·Jenkins·Harbor·Dashboard의 인증·권한·TLS·VPN·proxy, 브라우저 시각 점검과 신규 사용자 완주 시험은 수행하지 않았습니다.

RC5의 WSL Codex CLI 모의 호출 성공과 Claude Code OAuth 만료 결과는 과거 RC5 기록입니다. RC6의 실제 CLI 호환성 증거로 재사용하지 않습니다.
