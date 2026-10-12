# 등록에서 첫 AI 조회까지: 백엔드 구현 계획

작성일: 2026-10-10. 구현 상태 갱신: 2026-10-12. 상태: **target-free backend 구현·보안 검토 및 코드·기능 검증 통과; 두 desktop visual finding의 bounded finish review는 해당 범위에서 SHIP. 실제 서비스·OS vault·Desktop 앱 수용성은 검증되지 않음**. `go test -count=1 ./...` 통과(29.452초), `go vet ./...`, native build, focused/race suites 및 독립 보안 재검토가 통과했다. 최종-source CUA는 GitHub·Jenkins·Dashboard의 draft/check/review와 명시적 activation을 모두 통과했고 복원한 synthetic helper에서 readiness UI도 확인했다. 이전 등록·정책·활성화 3-state 캡처는 두 cosmetic 변경 전 source의 기능·시각 근거다. 변경 후 bounded review는 제목 영역과 operation-card 영역만 자연스러운 데스크톱 너비 1888px에서 다시 캡처해 검토했다. 두 cosmetic fix는 해당 영역에서 SHIP 판정을 받았고 clientWidth와 scrollWidth는 모두 1888px, console warning/error는 없었다. 전체 expanded flow와 변경되지 않은 다른 UI를 다시 점수화한 검토는 아니다. 2026-10-12 사용자 결정에 따라 모바일은 사용하지 않으며 mobile viewport/acceptance gate가 없다. 이전 expanded-flow의 **recapture** disposition과 390px 목표는 역사적 리뷰 기록으로 보존하되 현재 desktop review gate를 대신하지 않는다. RC3 ZIP build, MCPB 2.1.2 validate/pack/info, exact-ZIP MCP smoke, 전체 native lifecycle 검증이 통과했다(두 PowerShell 런타임에서 각각 30/30, 총 60/60). 최종 public release manifest/checksum은 문서 업데이트 뒤 release owner가 생성·검증하는 게시 단계다. 실제 공급자·Dashboard 배포 트랙·OS credential vault·Desktop 앱/클라이언트 호출은 이 문서에서 검증됐다고 주장하지 않는다. 입력 조건과 성공 기준은 [UX 결정 기록](UX_DECISIONS_2026-10-10.md)을 따른다.

## 통합 권고

첫 수직 슬라이스로 **명시된 서비스 종류와 URL을 받는 순수 URL 해석 함수, 로컬 미리보기 endpoint, 기존 대상 양식에 후보를 적용하는 URL-first UI**가 구현됐다. 파서에는 설정 파일, Credential Manager, 외부 HTTP 요청이 연결되지 않는다. 호스트만으로 GitHub와 Dashboard를 구분할 수 없으므로 서비스 종류는 사용자가 먼저 고른다. 결과는 편집 가능한 후보와 URL에서 빠진 필드만 담고, 입력 문자열 자체를 저장하거나 오류에 되돌려주지 않는다. 후보 적용은 메모리의 기존 서비스 양식에 값을 채우는 단계일 뿐 일반 초안 저장, 설정 저장, credential 저장, 연결 테스트를 하지 않는다.

확장 등록 흐름은 일반 초안, 저장 전 임시 연결 검사, 정책 검토, 활성 대상 저장을 기존의 대상별 saver에 연결해 구현했다. 일반 초안은 AI 제안 검토 기능이나 Disabled 대상에 넣지 않으며 별도 버전 sidecar에 저장한다. 활성 설정은 config v9를 유지하고 기존 대상·자격 증명 참조를 보존한다. 코드·기능 검증과 세 서비스의 synthetic helper lifecycle이 통과했다. 복원한 helper의 readiness와 이전 등록·정책·활성화 화면은 1888px 데스크톱 너비에서 확인했다. Cosmetic 수정 후 review는 상단/operation-card 영역의 두 visual finding만 대상으로 SHIP으로 마쳤으며, 나머지 expanded flow를 재점수화하지 않았다. 2026-10-12 결정에 따라 모바일 게이트는 없다. 실제 환경 경계는 아래에 별도로 적었다.

Dashboard 버전과 인증 트랙은 앱에서 감지하지 않으며 로그인 교환도 수행하지 않는다. 역사적 조사에서 legacy 전체 v1.7.0 소스의 로그인 토큰과 post-login JWE가 다른 점, 별도 web/v1.7.0 트랙이 있음을 확인했지만 실제 배포본 식별 근거는 아니다. 현재 draft check는 사용자가 명시적으로 제공한 bearer token으로 한 번의 Bearer GET을 시도하고 성공 응답만 검사의 성공으로 처리한다. 버전 숫자·인증 모드를 추정하거나 실패 후 다른 인증 방식으로 재시도하지 않는다. 이 synthetic/사용자 입력 경로는 private Dashboard 호환성을 검증하지 않는다.

## 현재 재사용할 코드와 빠진 연결부

| 단계 | 현재 코드 | 구현 내용 | 남은 검증 또는 환경 경계 |
| --- | --- | --- | --- |
| URL 후보 해석 | [registration_url.go](../../registration_url.go) parseRegistrationURL; 재사용 지점: [git_import.go](../../git_import.go) parseSafeGitRemoteURL, repositoryFromRemotePath; [json_remote_import.go](../../json_remote_import.go) parseSelectedJSONGitRemotes; [runbook_import.go](../../runbook_import.go) | 명시 서비스별 HTTPS URL 검증과 로컬 후보 추출. 기존 import는 선택한 Git 설정·Runbook·JSON을 미리보기만 하며 GitHub 후보를 만든다. URL-first parser는 import parsing과 별도 입력 경계다. | 첫 URL-first slice의 focused 기능 및 visual evidence는 별도 기록에 있다. 이 slice를 실제 서비스 접속으로 해석하지 않는다. |
| 등록 URL 미리보기 | [registration_url_ui.go](../../registration_url_ui.go) handleRegistrationURLPreview; [local_ui_handler.go](../../local_ui_handler.go) `/preview-registration-url`; [registration_url_ui_test.go](../../registration_url_ui_test.go) | 사용자가 명시적으로 선택한 POST에서 parser를 호출하고 안전한 후보/error DTO를 돌려준다. CSRF, 구성된 로컬 host, Origin, 단일 form 필드, query 거부와 부작용 없음 테스트가 있다. | endpoint는 URL shape만 미리 보며 초안·설정·Credential Manager를 건드리지 않는다. 저장 전 연결 검사도 하지 않는다. |
| 등록 UI와 onboarding lifecycle | [ui.html](../../ui.html), [REGISTRATION_UI_IMPLEMENTATION.md](REGISTRATION_UI_IMPLEMENTATION.md) | 첫 URL-first 단계는 서비스별 주소 확인, 검토 후보 적용, 기존 입력 보존, 취소, 오류, 현지화를 포함하며 기존 캡처의 scoped finish review는 SHIP이다. 확장 UI는 일반 draft, 명시적 check, policy review, activation, read status로 나뉘고 AI 제안 draft와 안내가 구분된다. 최종-source CUA에서 세 서비스 순차 draft/check/review/activation, credential/field invalidation, URL-selector preservation, check history locale, post-activation hint를 확인했다. 복원 helper에서 readiness도 확인했으며 browser overflow와 오류 로그가 없었다. | Current acceptance is desktop-only. Earlier registration, policy, and activation screenshots are functional/visual evidence from before the cosmetic-only source edit. The bounded finish review rescored the top and operation-card regions only, at natural 1888px desktop client width: both scored fixes passed with clientWidth=scrollWidth=1888px, no console warnings/errors, and no visible regression. The rest of the expanded UI was not rescored. The read-only prompt is 1044px wide, and the post-activation empty hint and stale preview are hidden. Earlier 1440px/390px capture attempts and **recapture** disposition remain historical; mobile is not used and is no gate. Do not extend the historical URL-first SHIP to this flow. See [live QA helper notes](REGISTRATION_UI_LIVE_QA.md). |
| 대상 저장과 활성화 복구 | [main.go](../../main.go) saveTargetWithNamedSecret; [jenkins.go](../../jenkins.go) saveJenkinsTargetWithNamedSecret; [dashboard.go](../../dashboard.go) saveDashboardTargetWithNamedSecret; onboarding activation controller | 기존 서비스별 saver를 재사용하고 stable target ID로 한 대상만 추가한다. focused adversarial/race 검증은 설정 쓰기·vault 삭제 실패, recovery marker 저장 실패 후 재시작, exact-target read-only finalize, precommit edit/delete를 포함한다. `ActivationRetry`는 기존 pending reservation에서만 켜지고, exact target이 없거나 일치하지 않으면 saver/예약 해제 전에 manual recovery로 반환한다. 최종 `go test -count=1 ./...`, vet, native build도 통과했다. | 테스트는 synthetic vault/config fixture이므로 실제 OS credential vault를 검증하지 않는다. 실사용 서비스 인증도 검증 범위 밖이다. |
| 연결 검사 | [main.go](../../main.go) testConnection, runConnectionTest, recordConnectionTest; [connection_test_tool.go](../../connection_test_tool.go); onboarding check controller | 저장된 대상 검사는 기존 동작을 유지하고, onboarding은 명시적 POST에서만 임시 credential로 GitHub·Jenkins·Dashboard candidate를 검사한다. Durable `CheckGeneration`은 같은 revision의 동시 검사를 순서화하고 오래된 완료의 receipt/permit 저장을 거부한다. 임시 검사는 `connection_tests`를 쓰지 않는다. 최종-source CUA에서 세 서비스 순차 check를 확인했다. | Fixture 기반 결과는 실제 endpoint, 권한, TLS/proxy 또는 provider 정책을 증명하지 않는다. |
| 설정 가져오기 | [settings_import.go](../../settings_import.go) handleSettingsImportPreview; [ui.html](../../ui.html) 설정 JSON 미리보기 | 기존 설정 파일 버전과 레코드 개수 미리보기 | 현재 동작은 미리보기 전용이며 대상 설정을 가져오거나 저장하지 않는다. 일반 URL 등록 초안 저장소로 재사용할 수 없다. |
| AI 제안 초안 | [parallel_contracts.go](../../parallel_contracts.go) SetupDraftSubmission 및 SetupDraftController; [setup_draft.go](../../setup_draft.go) 제출·검토·승인; [setup_draft_durable_queue.go](../../setup_draft_durable_queue.go) 큐 저장 | 버전 해시, 만료, CAS, 검토 후 명시적 승인, Credential Manager 비밀값 미포함이라는 패턴 | 이는 MCP가 제안한 대상·서비스 그룹·SSH 변경의 짧은 검토 큐다. 기본 만료 10분(최대 30분), 검토·승인·거절로 소비된다. 사람이 중단한 일반 등록 폼을 장기 저장하고 다시 여는 기능과 수명·상태가 다르다. |
| 준비 상태와 first read | [readiness.go](../../readiness.go) readinessStatusFromConfig, readinessHistoricalConnectionTest, inspectClients; [readiness.html](../../ui_fragments/readiness.html); onboarding read observer | 성공한 허용 MCP 외부 읽기만 `mcp_observed` receipt를 만들고, 필수 `DefinitionDigest`가 현재 안전한 대상 정의와 맞을 때 readiness에 반영한다. Catalog, check, 실패, service write, SSH/Python은 제외하고 user-reported 상태는 분리한다. | Backend tests cover current/stale receipt filtering and registration/check does not create receipts. Restored-helper CUA showed three enabled synthetic targets, no saved connection tests, missing Dashboard mapping, uninspected clients, and no observed MCP first reads. No external MCP read was performed; this proves the local synthetic UI state, not real-provider access or client identity. |
| 클라이언트 등록 | [runtime_clients_ui.go](../../runtime_clients_ui.go) runtimeClientsUIRegistrationSpecs; [client_registration.go](../../client_registration.go); [client_registration_runtime.go](../../client_registration_runtime.go) | 백업·계획·적용·검증·복원 및 실제 설정 파일 관측 | 현재 클라이언트 ID는 Codex, Claude, Gemini CLI다. Claude 어댑터를 Claude Desktop 지원으로 간주하거나, 등록 상태를 Desktop 연결·도구 호출 성공으로 간주하면 안 된다. ChatGPT Desktop용 등록 어댑터도 없다. 실제 사용자 Desktop/client first-read 증거는 없다. |

현재 설정은 [main.go](../../main.go)의 configVersion = 9 엄격한 JSON 스키마다. 기존 GitHub·Jenkins·Dashboard 대상은 Disabled가 설정된 완성형 등록 데이터다. validateTarget, validateJenkinsTarget, validateDashboardTarget은 ID와 자격 증명 참조까지 요구한다. 따라서 초안은 Disabled=true인 가짜 대상이 될 수 없다. 현재 GitHub·Jenkins·Dashboard 대상 수에는 설정 검증의 상한이 없다. 사용자의 1~5개 규모는 설계 규모로 반영하되, 새 하드 제한을 추가하지 않는 편이 좋다.

## 최소 백엔드 계약

### URL 해석

서비스 URL parsing 자체는 내부 순수 함수로 유지하고, UI에서는 로컬 미리보기 route가 이를 호출한다. 공개 외부 API나 대상 연결 동작은 parser에 넣지 않는다. 현재 구현 계약은 다음과 같다.

parseRegistrationURL(service, raw) → candidate, error

반환 후보는 서비스 종류와 추출 필드, URL에서 빠진 필드, 사람이 확인할 필드만 가진다. 파서는 네트워크·설정·비밀 저장소를 읽지 않는다. 사용자 정보·query·fragment·비 HTTPS·opaque URL·안전하지 않거나 잘못된 경로를 거부한다. 구문상 가능한 Jenkins job 꼬리와 배포 context path가 모호한 경우 parser는 가장 긴 정규 chain을 후보로 제시하되 두 분할 필드를 모두 필수 검토로 표시한다. 오류 응답은 입력 URL을 포함하지 않는다.

- GitHub는 HTTPS origin만 붙여넣어도 origin 후보를 만들고 저장소를 미완성으로 표시한다. 정확히 두 경로 조각인 저장소 경로는 선택적으로 추출하고, .git 접미사를 처리한다. 경로 접두부를 임의로 버리거나 URL을 조회하지 않는다. validateOrigin, validRepository, Git import의 경로 검증을 재사용할 수 있다. parseSafeGitRemoteURL은 SSH도 허용하므로 그대로 등록 URL 검증기로 쓰지 않는다.
- Jenkins는 서비스 선택 후 안전한 base/context URL을 받는다. 표준 job/<segment>/job/<segment>... 꼬리 경로를 가장 긴 정규 체인으로 해석해 base URL과 작업 경로 후보를 제시한다. Jenkins 배포 context path에도 같은 조각이 있을 수 있으므로 모든 추론 결과에서 base URL과 작업 경로를 모두 필수 검토 항목으로 표시한다. 작업 경로가 없으면 base URL만 채우고 작업 경로를 빠진 필드로 표시한다. 잘못되거나 불완전한 marker는 임의 분할하지 않고 origin만 제시하며 두 필드를 검토 대상으로 표시한다. validateJenkinsBaseURL, validJenkinsJobPath, safeJenkinsSegment를 재사용한다.
- Dashboard는 안전한 origin과 사용자가 복사한 전체 proxy/context path를 base URL 후보로 보존한다. /api/v1 경로를 자동 제거하거나 SPA fragment/query를 조용히 버리지 않는다. base URL 경로는 항상 사용자 검토가 필요하다. 이 파서는 Dashboard 버전이나 Bearer/JWE 인증 모드를 추론하지 않는다.

이 계약은 A/B 신규 대상 UI 선택과 독립적이며 parser, 로컬 미리보기 route, 기존 양식 적용 UI가 구현됐다. 미리보기는 사용자가 주소 확인을 눌렀을 때만 로컬에서 실행되며 저장 draft가 아니다. `MissingFields`는 URL에서 얻을 수 없는 URL 기반 리소스 필드만 뜻한다. 대상 이름·사용자 이름·인증 정보 같은 전체 등록 필드 완성 여부는 현재 기존 대상 양식이 보여주며, 미래의 초안 controller가 별도로 계산해야 한다. Jenkins의 정규 `/job/<name>` 꼬리 체인은 가장 긴 연속 체인으로 후보에 표시하되 배포 context path와 구별할 수 없어 `base_url`과 `job_path` 모두 검토 필드다. marker가 불완전하면 잘못된 URL로 자동 분할하지 않고 origin만 표시하며 검토를 요구한다.

### 일반 초안

사람이 직접 작성한 중단 가능 초안은 AI가 제출하는 SetupDraftReview와 별도 타입·컨트롤러로 둔다. 필수 필드가 빠진 상태도 저장할 수 있어야 하고, 저장·조회·수정·삭제가 있어야 한다. 초안 ID와 revision으로 CAS를 적용해 오래된 탭이 최신 입력을 덮지 않게 한다. 재시작 후에도 찾아 이어갈 수 있도록 짧은 만료 큐에 넣지 않는다.

초안은 config 경로에 묶인 제한 크기 JSON sidecar에 별도 schema version과 원자적 교체를 사용한다. 초안에는 원시 URL, 토큰·비밀번호, Credential Manager 참조, named-secret ID를 저장하지 않는다. 저장된 named secret을 사용하려면 재개한 뒤 다시 선택하고 현재 상태로 check해야 한다. 직접 입력한 비밀은 검사 POST 요청에서만 쓰고 바로 지운다. 초안을 삭제해도 활성 설정이나 Credential Manager 값은 건드리지 않는다.

상태는 저장된 필드의 완성도와 검사 receipt로 계산한다. 후보가 편집되거나 정책 구성이 바뀌면 기존 검사를 재사용하지 않는다. 초안 삭제·활성화 전까지 등록된 대상 목록, MCP 카탈로그, readiness의 활성 대상 수에는 나타나지 않는다.

### 저장 전 연결 검사

현재 testConnection에 URL이나 일반 폼 필드를 받아 임시로 등록하는 분기를 추가하지 않는다. 초안 컨트롤러에 별도 CheckDraft(draftID, expectedRevision, credentialInput) 계약을 두고, 초안 후보를 서비스별 검증 후 고정된 읽기 하나로 검사한다. 이 계약은 UI의 명시적 POST에서만 호출한다. 붙여넣기·URL 해석·화면 로드 시 네트워크를 호출하지 않는다.

요청에는 임시 토큰 또는 선택된 named-secret ID를 일시 전달할 수 있지만 초안 JSON·로그·오류·응답·receipt에는 ID나 secret value를 저장하지 않는다. 기존 자격 증명은 Credential Manager에서 한 번 읽어 요청 후 지운다. 검사는 현재 요청과 같은 서비스 읽기 함수와 동일한 제한·redirect 규칙을 쓰되, 아직 등록되지 않은 ID를 합성하지 않도록 대상 정의 검증과 저장 ID·참조 검증을 분리한다. 검사는 connection_tests를 쓰지 않는다. 성공 receipt는 draft revision 또는 후보 digest, 정책 digest, 시각, 안전한 결과 분류만 포함하며 후보 변경·보관 만료·설정 변경 후에는 새 검사를 요구한다.

역사적 조사에서 legacy full v1.7.0 소스의 login token과 post-login JWE가 다름을 확인했으며, 별도의 web/v1.7.0 트랙도 존재한다. 현재 앱은 배포 버전을 감지하거나 login exchange를 실행하지 않는다. Dashboard draft check는 사용자가 직접 제공한 token으로 명시적 Bearer GET을 시도하며, 성공 응답만 check 성공으로 처리한다. 버전·인증 방식을 자동 추정·전환하거나 실패 뒤 다른 방식으로 재시도하지 않는다. 따라서 synthetic check는 실제 private Dashboard 배포의 호환성을 증명하지 않는다.

### 정책 검토와 활성화

현재 대상별 허용 작업은 config의 독립적인 action 목록이 아니라 등록 대상과 서비스 코드가 정하는 고정 범위다. readinessStatusFromConfig와 registeredTargets가 활성 대상별 현재 노출 작업을 이미 열거한다. 정책 검토 초안은 동일한 효과를 보여주고, 후보·서비스·읽기 범위·추가로 매핑해야 나타나는 기능을 검토 digest에 묶어야 한다. 이 버전은 개별 MCP 작업을 선택·제외하는 기능이 아니다. 나중에 작업별 선택을 약속하려면 설정 필드뿐 아니라 각 MCP 호출에서 해당 권한을 강제하는 변경이 필요하다.

- GitHub 후보는 저장된 저장소에 대한 현재 github_repository, github_pull_request 읽기 범위를 명시한다.
- Jenkins 후보는 등록 작업·대기열·제한된 빌드 로그 읽기를 명시한다. 새 등록에서 작업 실행 사전 승인을 기본으로 켜지 않는다.
- Dashboard 대상만 등록하면 현재 namespace 읽기 범위가 노출된다. Deployment 상태·이벤트·Pod·로그 읽기는 서비스 그룹과 환경에 리소스 매핑을 추가한 뒤 가능하므로 같은 기능처럼 표시하지 않는다.

ActivateDraft는 draft revision, candidate/check/policy/config digests, current check generation, permit, and explicit policy confirmation을 재검증한 뒤에만 새 대상을 하나 저장한다. URL이 기존 대상과 같아도 자동 선택·덮어쓰기를 하지 않는다. 일반 onboarding activation은 create-only다. 기존 대상 수정은 별도 manual editor 경로에 남아 있으며 onboarding은 그 경로를 대체하거나 호출하지 않는다. 활성화는 다른 대상·서비스 그룹·SSH/Python 설정·named-secret metadata와 무관한 이력을 보존한다. 예약된 stable ID를 재사용해 복구하며, exact committed target만 read-only finalize를 허용한다.

### 첫 AI 읽기 receipt

registered_targets나 registered_service_bundles는 로컬 카탈로그이고, 폼의 연결 테스트는 외부 인증/연결 검사다. 둘 다 첫 AI 조회 receipt를 만들지 않는다. MCP 서버에서 등록된 외부 읽기 도구가 실제 데이터를 성공적으로 돌려준 직후만 앱 관측 이벤트로 기록한다. 대상 연결 검사, 실패 호출, 로컬 목록, SSH/Python, 서비스 쓰기 작업은 첫 서비스 읽기 근거에서 제외한다.

receipt에는 source=mcp_observed, 서비스 종류, 읽기 도구 이름, 시각, 필수 `DefinitionDigest`를 둔다. 서비스 URL·토큰·응답 내용·저장소/작업/리소스 이름은 저장하지 않는다. 현재 도구 호출 경로는 Desktop/CLI 제품 ID를 확정하지 않으므로 receipt에 ChatGPT Desktop, Claude Desktop 또는 Codex CLI라고 적지 않는다. 실제 앱에서 사용자가 결과를 확인했다고 보고하는 경로가 있다면 source=user_reported로 별도 보관하고, 관측 receipt를 생성하거나 대신하지 않게 한다. 기존 readiness의 브라우저 localStorage 확인은 이미 사용자 보고로 격리되어 있으므로 이 구분을 바꾸지 않는다.

readiness 응답에 앱 관측 first-read 상태와 사용자 보고 상태를 따로 노출한다. 성공 도구 이름과 시각만 보여주고 외부 응답은 포함하지 않는다. 이 기능은 등록·검사 UI 선택과 무관하지만, 실제 MCP 호출을 가짜로 성공시키는 통합 테스트는 만들지 않는다. 단위 테스트와 합성 HTTP 서비스 fixture에서 근거 생성 조건만 검사한다.

## 저장 형식과 기존 데이터 보존

권고안은 configVersion = 9의 활성 설정을 바꾸지 않고, 초안과 receipt를 connection-onboarding.json 같은 별도 파일의 자체 schema version 1로 저장하는 것이다. 이 파일은 기존 AI 제안용 setup-drafts.json과 수명·동작이 다르므로 파일명과 controller도 분리한다. 기존 migrateConfig가 v2-v9 대상과 참조를 현재 스키마로 검증하는 흐름을 건드리지 않는다. 선택한 설정 파일 가져오기는 [settings_import.go](../../settings_import.go)에서 v4-v9 레코드 수만 보여주는 preview이므로 초안·receipt를 다른 설치로 가져오지 않는다.

이는 활성 설정에 미완성 필드를 추가하지 않으면서 기존 대상 ID, Disabled, Dashboard/Jenkins 정책 값, named secret metadata, Credential Manager의 credential reference를 보존하는 최소 변경이다. 활성화는 기존 target-save 계열 코드로 수행한다. configWithSetupDraftState처럼 전체 대상 목록을 재구성하는 경로는 사용자 설정을 덮을 수 있으므로 일반 등록 초안의 활성화에 사용하지 않는다.

향후 초안을 정식 설정 파일에 포함하기로 결정하면 configVersion을 10으로 올리고 명시적 configV9 → v10 마이그레이션, v10 strict import-preview 지원, v9 모든 target·secret 참조 보존 검사를 추가해야 한다. 본 계획의 첫 구현에서는 이 분기를 피한다.

## 구현 배치와 병합 순서

1. **URL parser 기반 — 구현 완료.** 서비스 명시형 순수 함수와 표 기반 테스트가 추가됐다. GitHub origin·저장소 경계, Jenkins 가장 긴 정규 job 꼬리 후보와 검토 표식, Dashboard proxy prefix, query/userinfo/fragment/HTTP/잘못된 경로를 다룬다. A/B UI 선택과 독립적이다.
2. **URL-first 미리보기와 양식 적용 — 코드·기능·시각 검증 완료 (2026-10-11).** 로컬 preview route와 Connections 진입 양식은 service+HTTPS URL을 명시적으로 받고, shape-based 후보·누락·검토 필드를 보여준 뒤 사용자가 기존 서비스 저장 양식에 적용한다. Functional CUA는 세 서비스의 주소 확인·적용, 기존 대상 입력 보존, 새 대상 저장·취소, target picker guard, 오류와 현지화를 통과했다. 실제 production HTML을 isolated helper에서 열고 3개 서비스 화면과 기본 동작을 확인했다. Actual UI finish review 판정은 **SHIP**, in-scope 수정 사항 없음이다. Desktop inner 1440/clientWidth·scrollWidth 1422, mobile inner 390/clientWidth·scrollWidth 373, 사용자 inner 1051/clientWidth·scrollWidth 1033 모두 가로 overflow와 console error가 없었다. 이 흐름은 초안 저장, 연결 검사, 정책 승인, 설정 변경 또는 외부 서비스 접속을 하지 않는다. 이 역사적 판정은 현재 확장 흐름의 시각 수용 판정이 아니다.
3. **일반 초안 저장소 — 구현 및 focused 검증 완료.** 별도 sidecar envelope, CRUD/CAS, 재시작 후 이어하기, 삭제, 용량 제한, 비밀 없는 직렬화를 포함한다. 초안은 named-secret ID를 보관하지 않는다. AI 제안 초안과는 타입·저장소·UI 경계를 분리했다. 최종 full suite/build도 통과했다.
4. **저장 전 검사 — 구현 및 focused/race 검증 완료.** GitHub·Jenkins·Dashboard candidate를 명시적 검사 요청에서 검증한다. 비밀 미저장, timeout, 고정 오류 분류, safe redirect, 실패 시 config 불변, 동시 검사 generation 경계를 테스트한다. 합성 HTTP fixture 결과는 실제 endpoint 검증이 아니다.
5. **정책 검토와 활성화 — 구현, 복구/adversarial/race 검증 완료.** Candidate·policy·config digest와 check generation/permit을 검증하고 create-only로 한 대상만 추가한다. Exact target retry는 read-only finalize만 허용하며 absent/conflicting target은 manual recovery로 보낸다. Config write와 vault delete 실패, RecoveryRequired marker 실패 후 재시작, 반복 secret save 방지를 포함한 focused 증거가 있다. 독립 보안 검토에도 backend blocker가 없다. 최종 전체 suite/build도 통과했다.
6. **첫 읽기 receipt·readiness — 구현 및 focused/race 검증 완료.** 허용된 성공 MCP 읽기만 필수 DefinitionDigest를 포함한 receipt를 만들며, catalog/check/failure/write/local action은 제외한다. Readiness는 stale digest를 걸러내고 user-reported 상태는 분리한다. `TestDashboardDiagnosisMCPFirstReadReceiptRequiresSuccessfulSubread`는 실제 MCP diagnosis→durable sidecar→readiness 경로에서 all-failed는 receipt 없이 `not_observed`로, partial success는 현재 target definition digest에 bound된 receipt 하나와 이를 표시하는 readiness로 검증하며 focused 및 `-race -count=3` 실행이 통과했다. 합성 fixture는 실제 서비스 읽기 증거가 아니다.
7. **Desktop 패키지 — RC3 패키지와 native lifecycle 검증 완료.** Windows amd64 portable ZIP은 archive member 9개, 두 차례 빌드에서 동일한 executable, privacy marker 부재를 확인했다. MCPB CLI 2.1.2의 exact RC3 validate/pack/info, manifest/executable 두 파일 구성, version 0.1.0-rc.3 및 embedded executable parity를 확인했다. ZIP에서 추출한 실행 파일의 isolated stdio initialize와 tools/list(22 unique tools, exit 0)도 통과했다. 다섯 native lifecycle 그룹은 두 PowerShell 버전에서 각각 통과했다: basic lifecycle 7, link-path/reparse 12, recovery 9, root-creation reparse 1, root lease 1 (30/30 per runtime, 60/60 total). 최종 public release manifest와 checksum은 문서 커밋 뒤 release owner가 만들고 검증하는 게시 단계다. UI는 저장 뒤 활성 AI 앱에서 읽기를 확인하라고 안내한다. 실제 Desktop 설치·등록·도구 호출, 실제 provider endpoint, Dashboard 배포/auth 트랙, OS credential vault는 검증되지 않았다. 공식 문서에 적힌 지원 능력과 실제 사용자의 설치·호출 증거를 분리한다.

두 cosmetic 수정 뒤 bounded review에서 상단과 operation-card 영역은 자연스러운 1888px 데스크톱 너비로 확인됐다. clientWidth와 scrollWidth는 모두 1888px이며 console warning/error와 visible regression은 없었다. 앞선 등록·정책·활성화 3-state 캡처는 수정 전 source의 기능·시각 근거이며 전체 UI를 다시 점수화하지 않았다. 1888px readiness CUA는 세 개의 enabled synthetic target, saved connection test 없음, Dashboard mapping 없음, client inspection 미완료, observed MCP first read 없음을 보였다. 화면 가로 overflow와 browser logs는 없었고, read-only prompt는 1044px이며 post-activation empty hint와 stale preview는 숨겨져 있다. 2026-10-12 이후 모바일은 사용하지 않는 것으로 기록됐으며 390px/mobile 캡처는 current acceptance gate가 아니다. 과거 `ERR_CONNECTION_REFUSED`는 10분 기본 Go test timeout이 helper를 종료한 운영 문제였다. replacement launcher는 `-timeout=13h`로 실행해 12시간 review timer보다 길게 유지한다. 동일한 settings path 재사용 시 ordinary onboarding sidecar도 그대로 유지되며 관측 상태는 0 drafts, 0 pending activations, 3 completed activations, 0 first-read receipts였다. 이후에도 Dashboard 배포/auth 트랙, 실제 provider 권한과 연결, 실제 OS credential vault, 선택한 Desktop/CLI의 first-read는 사용자의 실제 환경에서 별도 검증해야 한다.

## 아직 남는 실제 환경 근거

합성 테스트는 URL 분해, 저장 불변성, 인증 헤더, 오류 처리, receipt 생성 조건을 검증할 수 있다. 실제 회사 URL의 경로·TLS·권한·Dashboard 배포 트랙과 proxy 인증 변환, 토큰 발급 정책, Desktop 앱이 stdio 서버를 실제 호출하는지는 알려주지 않는다. 테스트 성공은 실제 연결 검증으로 표기하지 않고, 첫 read receipt의 앱 관측과 사용자 보고를 별도 근거로 유지한다.

## URL-first 첫 단계의 검증 근거

**첫 URL-first 수직 슬라이스의 코드가 통합됐다.** `registration_url.go`와 `registration_url_test.go`는 서비스 종류를 명시적으로 받는 내부 순수 parser와 합성 입력 테스트를 제공한다. 파서는 외부 요청·설정 저장·Credential Manager 접근을 하지 않고, 오류에 원시 URL을 포함하지 않는다. GitHub는 HTTPS origin과 정확한 저장소 경로를 처리하고, Jenkins는 가장 긴 정규 job 꼬리 경로를 후보로 삼되 모든 추론된 base URL/job path를 필수 검토로 표시한다. Dashboard는 proxy/context path를 보존해 검토 대상으로 표시하며 버전이나 인증 모드는 추론하지 않는다.

`registration_url_ui.go`의 `POST /preview-registration-url`은 같은 parser를 CSRF 및 로컬 Origin/Host 보호 아래서 호출한다. form 필드와 query를 엄격히 제한하고, 후보 외에는 고정 오류를 반환한다. `ui.html`의 Connections URL-first 입력은 사용자가 주소 확인을 요청할 때만 이 로컬 route를 호출한다. 후보는 검토 후 기존 대상 양식에 수동 적용되며 저장·비밀 저장·외부 접속은 하지 않는다. 편집 중인 기존 대상 값은 새 대상으로 전환할지 확인하기 전까지 유지된다. 이 UI 동작은 별도 [등록 UI 구현 기록](REGISTRATION_UI_IMPLEMENTATION.md)에 정리되어 있다.

검증 상태:

- `go test -count=1 ./...` — 첫 URL-first wave 기록의 역사적 실행, 통과, 29.121초.
- Focused privacy/template acceptance — 통과.
- `go vet ./...` — QA helper 추가 뒤 통과.
- Native Go build `go build -trimpath -ldflags '-s -w'` — 산출 실행 파일 생성·검증 통과.
- Diff check — clean.
- Functional CUA — 세 서비스의 URL 확인→후보 적용, 편집 중인 기존 데이터 보존, 새 대상 저장 및 취소, target picker 차단, 오류 상태와 현지화 통과.
- Visual review (2026-10-11) — actual UI finish disposition **SHIP**, material fix 없음. Exact production HTML을 isolated local helper에서 열어 desktop inner 1440, mobile inner 390, 사용자 inner 1051/clientWidth·scrollWidth 1033을 확인했다. 각각 1422, 373, 1033으로 horizontal overflow가 없고 console error도 없었다. 세 캡처 모두 full-scope review를 통과했다.

Focused and race tests, the latest full uncached Go suite (`go test -count=1 ./...`, 29.452s), vet, native build, and independent security review pass for the current local implementation. Final-source CUA passed sequential draft/check/review and explicit activation for all three services; restored-helper CUA verified readiness as recorded above. Current UI acceptance is desktop-only: the bounded visual SHIP disposition covers only the two scored cosmetics at natural 1888px client width. Final-source captures of the top and operation-card regions had clientWidth=scrollWidth=1888px, with no console warnings/errors or visible regression; earlier registration/policy/activation captures are prior-source evidence and the rest of the interface was not rescored. The read-only prompt is 1044px wide, and the post-activation empty hint and stale preview are hidden. The older 1440px/390px recapture disposition is historical and mobile is not a current gate. RC3 ZIP, exact MCPB validate/pack/info, MCP smoke, and all five native lifecycle groups passed on both PowerShell runtimes (30/30 each, 60/60 total). The final public release manifest and checksum are generated and verified by the release owner after this documentation update. These local gates do not establish access to a real service, its permissions or Dashboard auth track, storage in the user's OS credential vault, or an actual Desktop/CLI first read. Keep URL preview, saved-target connection tests, and successful MCP reads as distinct evidence categories.
