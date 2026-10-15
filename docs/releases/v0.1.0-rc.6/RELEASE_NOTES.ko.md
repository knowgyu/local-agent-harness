# v0.1.0-rc.6

Windows x64용 사전 공개 후보입니다. RC6의 실제 테스트·패키지 결과는 별도 `VALIDATION_SUMMARY.ko.md`에 기록합니다.

- MCP `setup_status.configuration_revision`을 초안 제출이 요구하는 전체 설정 revision과 맞췄습니다. 최신 MCP 상태 조회에서 받은 값을 `submit_setup_draft.base_revision`에 그대로 사용합니다. HTTP 준비 상태 화면의 revision은 범위가 다르므로 `base_revision`으로 사용할 수 없습니다.
- 에이전트 안내를 실제 초안 흐름에 맞췄습니다. 필요한 named credential은 제출 전에 UI에서 저장하고 제안에는 비밀이 아닌 이름만 넣습니다. MCP 응답의 `id`를 UI `draft_id` 입력란에 넣고 `base_revision`, `digest`를 함께 입력해 제안을 열어 승인하거나 거부합니다. 화면에는 자동 제안 목록, credential 선택기, 제안 편집기가 없습니다.
- 새 대상과 새 대상 ID를 참조하는 서비스 묶음 매핑은 단계별로 제출합니다. 대상을 먼저 승인한 뒤 최신 상태에서 발급된 ID와 revision을 확인하고 매핑 제안을 만듭니다.
- 기본 `setup_status`는 서비스에 접속하거나 자격 증명을 불러오지 않습니다. 최초 호출 때 보호된 로컬 onboarding 네임스페이스 디렉터리를 만들거나 보호 설정을 보완할 수 있으며 설정이나 영수증 레코드는 변경하지 않습니다.
- `AGENT_SETUP.md`는 portable ZIP 바깥의 별도 릴리스 첨부 파일입니다. `CLIENT_SETUP.md`는 ZIP에 포함되어 오프라인 클라이언트 안내로 사용할 수 있습니다.

## 검증 범위

RC5의 WSL Codex CLI 모의 서비스 호출은 RC5에만 해당하는 과거 증거입니다. RC5의 Claude Code 시도는 OAuth 만료로 도구 호출 전에 중단되어 성공을 확인하지 못했습니다. 어느 결과도 RC6 검증으로 재사용하지 않습니다. RC6 테스트·패키지·클라이언트 결과는 `VALIDATION_SUMMARY.ko.md`의 실제 기록만 기준으로 확인하세요. 모의 서비스나 프로토콜 fixture는 실제 서비스 연결 또는 사용자 환경 수용을 입증하지 않습니다.
