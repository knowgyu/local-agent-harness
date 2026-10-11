# v0.1.0-rc.3 사용자 수용 체크리스트

이 체크리스트는 RC3의 로컬 데스크톱 흐름을 확인하고, 선택한 경우에만 별도 비운영 서비스·클라이언트 검증을 기록합니다. UI 수용 범위는 데스크톱입니다. 모바일은 사용하지 않으며 모바일 브라우저나 모바일 캡처는 요구하지 않습니다. 폭이 좁은 창도 데스크톱 창입니다.

각 항목은 `통과`, `실패`, `미확인` 중 하나로 표시하세요. 합성 helper 결과, 실제 서비스 연결, 클라이언트 등록, 실제 MCP read는 서로 다른 증거입니다. 비밀값·실제 URL·사용자 이름·개인 경로·고객/회사 데이터를 checklist, issue, screenshot, log에 적지 마세요. 비운영 환경이 없으면 실제 환경 항목은 `미확인`으로 남깁니다.

## 1. 다운로드와 데스크톱 UI

- [ ] Releases의 실제 asset 이름이 `release-manifest.json`에 있다.
- [ ] 받은 파일 각각의 SHA-256이 함께 제공된 `SHA256SUMS.txt`와 일치한다. 값이 다르면 실행하지 않는다.
- [ ] 패키지가 unsigned prerelease임을 확인했다. Checksum은 파일 무결성 확인이지 게시자 인증이 아니다.
- [ ] 새 폴더에 ZIP 전체를 압축 해제하고 설치/실행했다.
- [ ] Local UI는 loopback desktop window에서 열린다. 이 주소를 MCP 서버 URL로 사용하지 않는다.
- [ ] 실제로 사용하는 현재 데스크톱 창 크기에서 페이지가 잘리고 가로로 넘치지 않는지, 레이블·키보드 포커스·오류 문구를 확인했다. Narrow desktop windows remain desktop usage.
- [ ] UI 기본 한국어와 영어 전환, 일반 draft 진입, 연결된 등록·운영 화면 접근을 확인했다.

## 2. URL과 일반 draft

테스트할 서비스를 선택하고 실제 또는 비운영 URL만 직접 입력합니다. 외부 연결을 원하지 않으면 `.invalid` synthetic helper만 사용하세요.

- [ ] GitHub Enterprise, Jenkins, Dashboard 중 선택한 서비스의 URL-first 입력이 후보 필드를 제안한다. URL parsing 자체는 외부 요청을 보내지 않는다.
- [ ] candidate의 base/context path, repository/job/namespace 등 추론 내용을 직접 확인하고 필요한 값을 수정했다.
- [ ] 일반 draft 저장 후 화면을 새로 열어 resume/delete할 수 있다. 미완성 draft는 활성 target이나 MCP catalog/readiness의 활성 항목이 아니다.
- [ ] draft 저장 데이터에 원본 URL, URL query/fragment, password/token, Credential Manager reference, named-secret ID가 포함되지 않는다.
- [ ] 기존 named secret을 선택해 쓰는 경우 draft resume 후 다시 선택하고 새로 check해야 한다.

## 3. 임시 check, policy review, activation

- [ ] **Check** 버튼을 누르기 전에는 서비스 요청이 없음을 확인했다. Check는 명시적으로 눌렀을 때만 호출된다.
- [ ] Check 결과는 후보 검증이다. 활성 target을 만들거나 saved-target connection history를 기록하지 않는다.
- [ ] check 후 candidate/credential을 바꾸면 이전 review/permit이 무효화되어 다시 확인해야 한다.
- [ ] policy review에서 직접 포함되는 읽기 범위와 추가 mapping이 있어야 가능한 기능을 검토했다. Jenkins 실행 권한 등 범위 밖 작업을 읽기 기능처럼 취급하지 않았다.
- [ ] 사용자 확인 후 activation으로 새 target 하나만 추가했다. Onboarding은 기존 target을 선택·수정·덮어쓰지 않는다.
- [ ] Activation 뒤 saved-target connection test, MCP catalog, 성공한 외부 MCP read를 서로 다른 상태로 확인했다.

Recovery 경고가 나타나거나 결과가 모호하면 activation을 반복하거나 target을 수동 삭제하지 마세요. Config와 sidecar 상태 파일을 보존한 채 추가 조작을 멈춥니다. 복구 동작은 synthetic fault-injection tests에서 확인했으며, production service 장애 복구는 시험하지 않았습니다.

## 4. 실제 MCP first read (선택 검증)

- [ ] 클라이언트에 실행 파일과 `--mcp` stdio command를 수동 등록했다. 설정 저장만으로 서버 연결을 통과했다고 기록하지 않는다.
- [ ] 해당 클라이언트의 실제 세션에서 server와 tools를 확인했다.
- [ ] `registered_targets` 같은 catalog 도구를 호출했다. 이 호출은 로컬 설정 목록이며 external service read는 아니다.
- [ ] 선택한 비운영 target에서 읽기 전용 MCP tool을 실제로 호출해 서비스 데이터를 성공적으로 반환받았다.
- [ ] `/readiness`의 `observed MCP first reads`에 성공한 permitted read가 표시되는지 확인했다. User-reported 상태나 connection check는 `mcp_observed`가 아니다.

영향이 있을 수 있는 외부 요청은 target과 권한을 먼저 확인하세요. 실제 MCP read를 하지 않았다면 first-read 항목을 미확인으로 둡니다. `mcp_observed` receipt도 실제 client identity를 확정하지는 않습니다.

## 5. OS credential 및 Desktop extension 경계

- [ ] 실제 Windows Credential Manager 저장·resolve·rotation·cleanup은 별도 비운영 계정/target에서 확인했다. RC3 자동 검증은 synthetic vault fixture만 사용했다.
- [ ] ChatGPT Desktop, Claude Desktop, Codex CLI 중 사용한 클라이언트에서 실제 server/tool visibility와 call을 별도로 확인했다. 다른 클라이언트 결과로 대체하지 않는다.
- [ ] Claude Desktop `.mcpb`는 unsigned preview package다. MCPB CLI validation/packing은 Desktop install, launch, tool visibility, credential access, 또는 actual MCP call을 입증하지 않는다.

| 확인 영역 | 결과 | 비밀 없는 메모 |
| --- | --- | --- |
| 다운로드/체크섬/설치 | 통과 / 실패 / 미확인 | |
| 데스크톱 UI와 현재 창 크기 | 통과 / 실패 / 미확인 | |
| URL/draft/check/review/activation | 통과 / 실패 / 미확인 | |
| actual OS credential store | 통과 / 실패 / 미확인 | |
| 실제 client server/tool/call | 통과 / 실패 / 미확인 | |
| actual MCP external first read | 통과 / 실패 / 미확인 | |
| update/uninstall | 통과 / 실패 / 미확인 | |

**남은 미확인 항목:**

-
