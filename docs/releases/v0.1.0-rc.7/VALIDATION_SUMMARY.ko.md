# v0.1.0-rc.7 검증 요약

RC7 소스와 배포물의 검증 기록입니다. RC6 통과 결과를 RC7 결과로 재사용하지 않습니다.

## 주소와 작업명 회귀 검사

수정 전 소스에 새 검사를 적용했을 때 일반 GitHub의 API 주소와 Jenkins의 공백·한글·괄호·`+` 작업명에서 의도한 실패를 확인했습니다. 수정 후 집중 검사는 WSL의 Go 1.26.7에서 통과했습니다.

GitHub 9가지 주소 조합에서 저장소 읽기, PR 읽기, 초기 연결 시험이 정확한 호스트·포트·경로·인증을 사용하는지 확인합니다. 일반 GitHub, 기본 HTTPS 포트, 사내 호스트, 별도 포트, github.com처럼 보이는 다른 호스트와 비표준 공개 호스트 포트를 포함합니다. 공개 GitHub 링크도 웹 주소로 반환합니다.

Jenkins 3가지 작업명 조합에서 인코딩 URL·일반 공백/한글 URL 후보, 등록 작업 읽기, 초기 연결 시험, 실행·빌드·로그 경로와 queue의 작업·호스트 결합을 검사합니다. 경로 순회, 이중 인코딩, 숨은 경로 구분자, 제어 문자와 잘못된 UTF-8 거부도 검사합니다. 모든 서비스 요청은 모의 전송이며 실제 서버에 접속하지 않았습니다.

## Windows 네이티브와 배포물

| 검사 | 결과 |
|---|---|
| 집중 회귀 검사 | Windows Go 1.26.7 windows/amd64 통과, 9.28초 |
| 전체 테스트 | 같은 네이티브 환경의 `go test -count=1 ./...` 통과, 37.82초 |
| 정적 검사 | `go vet ./...` 통과, 1.10초 |
| 최종 Windows ZIP | 빌드 통과, 21.48초; 평면 구성원 11개, 두 번 빌드한 EXE 바이트 일치, 개인정보 표식 검사 통과 |
| 오프라인 문서 | ZIP에 복사한 문서가 원본과 일치하며 상대 파일 링크가 패키지 안에서 해석됨 |
| Claude Desktop MCPB | 고정된 `@anthropic-ai/mcpb` 2.1.2 validate·pack·info 통과, 13.45초; manifest와 EXE만 포함 |
| EXE 동등성 | 문서 검토 후 다시 만든 최종 ZIP의 EXE가 MCPB의 EXE와 네이티브 stdio 검사에 사용한 EXE와 바이트 일치 |
| 패키지 네이티브 stdio | 프로토콜 `2025-11-25`, 고유 도구 23개, 원본과 같은 안내 리소스, 등록용 설정 버전, 잘못된 인수 거부 확인 |
| 기본 상태 조회 | 격리한 Windows 프로필에서 데이터 파일 변경 없이 보호된 증거 디렉터리 2개 생성 |
| 설치·업데이트·복구 | 최종 ZIP으로 Windows PowerShell 5.1.26100.9711 및 PowerShell 7.6.7에서 12/12 통과; 재분석 경로 거부·루트 잠금·중단 복구·이전 저널·정확한 설치/업데이트/제거·공개 RC5 → RC7 업그레이드 포함 |
| 공개 RC6 → 최종 RC7 업그레이드 | Windows PowerShell 5.1.26100.9711 및 PowerShell 7.6.7에서 2/2 통과; 설치 파일·영수증 일치와 사용자 파일 보존 확인 |

각 PowerShell에서 중단 복구 9건과 이전 저널 3건을 확인했습니다. 이전 저널 검사의 RC4 이름 매개변수에는 해시를 고정한 RC7 배포물을 전달했습니다. 이는 RC3 → RC7 호환성 검사이며 실제 RC4 배포물 검사가 아닙니다.

서비스와 자격 증명 저장소는 모의 환경이며, 실제 CLI·Desktop 클라이언트나 브라우저 수용 시험 결과가 아닙니다. stdio 검사는 패키지 실행 파일에 프로토콜 요청을 직접 보낸 검사입니다.

최초 PowerShell 7 테스트 실행기가 Windows PowerShell 5.1에 모듈 환경을 전달해 `Get-FileHash`를 찾지 못한 실패는 제품 동작 전의 실행기 문제로 기록했습니다. 실행기를 Windows PowerShell에서 시작하도록 고치고, README의 PowerShell 7 → `powershell.exe` 직접 실행 경로도 따로 확인했습니다. 실패한 실행을 통과 작업 수에 합산하지 않습니다.

배포 EXE는 `-mod=readonly -trimpath -buildvcs=false -ldflags="-s -w"`로 빌드했습니다. 파일명·크기·SHA-256과 소스 입력의 식별값은 `release-manifest.json` 및 `SHA256SUMS.txt`에서 확인하세요.

## 미검증 범위

실제 회사의 GitHub/Jenkins/Harbor/Dashboard 버전·권한·VPN·프록시·인증서·SSO, Windows Credential Manager 저장·삭제, 실제 Codex·Claude CLI 및 Desktop 설치·도구 호출, 새 사용자 브라우저 사용성은 검증하지 않았습니다. RC5의 클라이언트 실행 기록은 과거 RC5 결과이며 RC7 결과가 아닙니다.
