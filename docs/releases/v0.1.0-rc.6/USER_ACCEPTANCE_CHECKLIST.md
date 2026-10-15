# v0.1.0-rc.6 사용 확인

사용 환경에서 각 항목을 통과·실패·미확인으로 기록하고, 모의 서비스 결과와 실제 서비스 결과를 구분합니다. 이 체크리스트는 RC6 결과를 대신하지 않습니다.

## 설치와 클라이언트

- [ ] ZIP 이름·크기·SHA-256이 RC6 manifest 및 checksum 파일과 일치합니다.
- [ ] 압축을 푼 폴더의 설치 스크립트로 앱을 설치하고 UI를 시작했습니다.
- [ ] 앱의 `/guide` 리소스와 RC6 별도 `AGENT_SETUP.md` 안내를 확인했습니다.
- [ ] 대상 CLI에 MCP 서버를 등록하고 새 세션에서 도구가 보이는지 확인했습니다.

## 안전한 제안 흐름

- [ ] 에이전트는 사용자가 지정한 정확한 경로만 읽고 선택하지 않은 비밀 파일을 보지 않았습니다.
- [ ] 필요한 named credential은 제안을 제출하기 전에 로컬 UI에서 저장하거나 선택했습니다.
- [ ] 제안에는 비밀이 아닌 credential 이름만 포함했고, secret 값이나 credential reference는 포함하지 않았습니다.
- [ ] 설정이나 credential이 바뀐 뒤 MCP `setup_status`를 다시 호출하고 최신 `configuration_revision`을 `base_revision`에 사용했습니다.
- [ ] 응답의 `id`를 UI의 `draft_id` 입력란에 넣고 `base_revision`과 `digest`도 `/ssh-settings#setup-draft-review-heading`에 입력해 해당 제안 전체를 확인한 뒤 승인 또는 거부했습니다. UI가 제안 목록을 자동 표시하거나 누락 항목을 편집한다고 가정하지 않았습니다.
- [ ] 새 대상을 먼저 승인하고, 상태를 새로 조회한 뒤 반환된 ID와 revision으로 서비스 묶음 매핑을 별도 제출했습니다.
- [ ] 중단되거나 만료된 제안을 다시 제출하기 전에 기존 검토 상태를 확인했습니다.

## 대상 읽기와 증거

- [ ] 각 실제 MCP 읽기의 정확한 대상·범위에 대해 사용자가 허용했습니다.
- [ ] 연결 시험 및 카탈로그 조회와 실제 대상 서비스 읽기를 구분했습니다.
- [ ] 각 대상별 `first_read.state`를 확인하고 `observed`, `not_observed`, `unavailable` 의미를 구분했습니다.
- [ ] `mcp_observed` 영수증을 클라이언트 신원 증명으로 표현하지 않았습니다.
- [ ] RC5 CLI 결과를 RC6의 클라이언트 성공 또는 실제 서비스 검증으로 재사용하지 않았습니다.
- [ ] 실제 사내 서비스 연결, OS 자격 증명 저장, Windows 클라이언트 등록은 별도 실사용 결과로 기록했습니다.

## 환경별 결과

| 환경 또는 대상 | 통과·실패·미확인 | 실제로 관찰한 근거 |
|---|---|---|
| Windows 설치와 앱 실행 |  |  |
| Codex CLI MCP 등록·도구 표시 |  |  |
| Claude Code CLI MCP 등록·도구 표시 |  |  |
| 사용자 지정 서비스의 허용된 MCP 읽기 |  |  |
| Windows Credential Manager 저장·사용 |  |  |
