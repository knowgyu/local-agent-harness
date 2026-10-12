# Local Agent Harness

Local Agent Harness는 이름 있는 개발 서비스 대상을 등록하고, loopback 웹 UI에서 관리하며, 등록된 대상과 허용된 작업만 MCP로 제공하는 Windows 앱입니다.

## v0.1.0-rc.4

[빠른 시작](docs/releases/v0.1.0-rc.4/START_HERE.ko.md), [릴리스 노트](docs/releases/v0.1.0-rc.4/RELEASE_NOTES.ko.md), [검증 요약](docs/releases/v0.1.0-rc.4/VALIDATION_SUMMARY.ko.md), [수용 체크리스트](docs/releases/v0.1.0-rc.4/USER_ACCEPTANCE_CHECKLIST.md), [등록부터 첫 읽기까지 사용 안내](docs/USER_GUIDE.ko.md)를 확인하세요. Windows x64 압축 파일은 Releases 페이지에서 받고 `SHA256SUMS.txt`와 `release-manifest.json`에서 자산 이름과 해시를 대조하세요. RC4는 서명되지 않은 사전 공개 후보입니다.

## 기능과 경계

- 이름이 지정된 서비스 대상과 허용 작업을 로컬에 저장하며, MCP 요청마다 저장된 범위를 확인합니다.
- 외부 서비스 비밀값은 Windows Credential Manager에 저장하고, 설정에는 참조만 둡니다.
- Windows 현재 사용자 환경변수를 별도 기능으로 관리할 수 있습니다. 같은 Windows 계정으로 실행 중인 프로세스는 해당 값을 읽을 수 있습니다.
- 로컬 UI와 HTTP listener는 loopback에만 바인딩됩니다. 제공되는 MCP 진입점은 stdio입니다 (local-agent-harness.exe --mcp).
- 반환 데이터의 크기와 개수를 제한하고 알려진 민감 필드를 best-effort 방식으로 필터링합니다. 모든 비밀을 찾아낸다고 보장하지 않습니다.
- RC4는 앱 안에서 열 수 있는 한국어·영어 읽기 전용 사용 안내(`/guide`)와 ZIP에서 오프라인으로 볼 수 있는 한국어 안내서를 제공합니다. 예시를 복사해도 연결이나 MCP 호출은 실행되지 않습니다.

제품은 GitHub Enterprise, Jenkins, Harbor, Kubernetes Dashboard REST, SSH 및 등록된 로컬 작업 흐름을 포함합니다. 정확한 범위와 제한은 릴리스 문서에 설명되어 있습니다. 실제 서비스와 클라이언트의 호환성은 사용 환경에서 확인해야 합니다. 합성 검사는 실제 환경 호환성을 입증하지 않습니다.

현재 UI 사용·검증 범위는 데스크톱입니다. 모바일은 사용하지 않으며 모바일 뷰포트는 현재 지원 목표나 수용 기준이 아닙니다. 좁은 데스크톱 창은 데스크톱 사용 범위에 포함됩니다.

## 프로젝트 문서

- [제품 요구사항](docs/PRD.md)
- [아키텍처](docs/ARCHITECTURE.md)
- [보안 경계](docs/SECURITY.md)
- [UX 원칙](docs/UX.md)
- [이럴 때 이렇게 쓰세요: 등록부터 첫 MCP 읽기까지](docs/USER_GUIDE.ko.md)
- [MCP 클라이언트 수동 설정 예시](docs/CLIENT_SETUP.md)
- [RC3 이전 릴리스 문서](docs/releases/v0.1.0-rc.3/RELEASE_NOTES.ko.md)

설계 문서는 요구사항과 제안 사항을 담고 있습니다. 현재 동작과 이번 릴리스에서 확보한 근거는 릴리스 문서에서 확인하세요.
