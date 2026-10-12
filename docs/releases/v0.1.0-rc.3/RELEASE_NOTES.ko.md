# Local Agent Harness v0.1.0-rc.3

Windows x64 사전 공개 후보입니다. 설치 패키지는 서명되지 않았습니다. 게시된 파일명과 SHA-256은 `release-manifest.json`과 `SHA256SUMS.txt`에서 확인하세요. 해시는 다운로드 파일의 동일성과 손상 여부를 대조하지만 게시자 신원을 증명하지 않습니다.

## 주요 변경

- GitHub Enterprise, Jenkins, Kubernetes Dashboard를 대상으로 URL에서 안전한 후보를 제안하고, 사용자가 검토한 뒤 일반 onboarding draft로 저장할 수 있습니다.
- 일반 draft는 활성 연결 대상과 별도 sidecar에 보관합니다. 초안에 원본 URL, 비밀번호·토큰, Credential Manager 참조, named-secret ID를 저장하지 않습니다. 저장된 기존 secret을 사용하려면 다시 선택하고 새로 검사해야 합니다.
- 서비스 요청은 사용자가 명시적으로 누른 임시 check에서 시작합니다. Check는 저장 전 후보를 검사하고 일반 connection history를 쓰지 않습니다. Check 성공만으로 target을 저장하지 않습니다.
- 고정 범위의 policy review 후에만 사용자가 명시적으로 activation할 수 있습니다. Onboarding activation은 새 target 하나를 만드는 경로이며, 기존 target 편집은 기존 수동 편집 흐름에 남아 있습니다.
- 실제 허용 MCP 외부 읽기가 성공한 뒤에만 `mcp_observed` first-read receipt를 만들며, receipt는 현재 안전한 target definition의 필수 digest에 묶입니다. catalog 조회, connection check, 실패, 로컬 테스트, 서비스 쓰기는 first-read가 아닙니다.
- Activation recovery는 stable target ID를 재사용합니다. 재시작 뒤 exact committed target이 확인되는 경우에만 read-only finalize하며, target이 없거나 일치하지 않으면 수동 복구 안내로 멈춥니다. 합성 fault-injection 테스트는 recovery marker 기록 실패 후에도 secret을 두 번 저장하지 않음을 확인합니다.
- RC3 문서는 onboarding 순서, 실제 MCP first-read와 connection check의 차이, 검증 경계를 설명합니다.

## 데스크톱 UI 범위

2026-10-12 사용자 결정에 따라 UI 사용·수용 범위는 데스크톱입니다. 모바일은 사용하지 않으며 모바일 뷰포트는 지원 목표나 릴리스 게이트가 아닙니다. 폭이 좁아진 데스크톱 창도 데스크톱 사용입니다. 두 scored desktop cosmetic fixes의 bounded visual review는 SHIP 판정을 받았고, 상단·operation-card 영역은 자연스러운 1888px 너비에서 overflow나 console warning/error 없이 확인됐습니다. 이 제한된 판정은 전체 expanded flow를 다시 시각 평가한 결과가 아닙니다.

## 설치와 AI 클라이언트

다운로드·체크섬 확인·설치·업데이트·제거는 [Windows 시작 안내](START_HERE.ko.md)를 따르세요. MCP 클라이언트 설정은 수동이며 실행 파일을 stdio로 시작합니다. UI의 loopback 주소를 MCP 서버 URL로 입력하지 않습니다. Claude Desktop용 `.mcpb` preview는 별도의 unsigned package 경로이며, 로컬 패키지 검증은 Claude Desktop 설치·실행·도구 표시·호출을 입증하지 않습니다.

## 확인하지 않은 환경

RC3 코드 검증은 synthetic fixtures와 local helper를 사용했습니다. 실제 GitHub Enterprise/Jenkins/Dashboard 서비스·권한·Dashboard deployment/auth track, 실제 Windows Credential Manager 저장·삭제, 그리고 ChatGPT Desktop·Claude Desktop·Codex CLI에서의 실제 도구 호출은 검증되지 않았습니다. `mcp_observed` 경로는 합성 MCP 호출과 durable receipt/readiness 테스트를 통과했지만, 이 RC의 CUA에서 외부 서비스에 실제 MCP first read를 수행하지 않았습니다. 실제 환경 근거는 [검증 요약](VALIDATION_SUMMARY.ko.md)과 별도로 구분하세요.
