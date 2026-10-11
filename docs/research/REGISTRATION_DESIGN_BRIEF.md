# Registration UX comparison design brief

작성일: 2026-10-11. 상태: **새 연결 A/B 비교 prototype은 보관용. 실제 URL 기반 등록 흐름은 `ui.html`에 구현됐다. 2026-10-11 URL-first visual finish review의 SHIP 판정은 해당 첫 wave에만 적용**.

## Current UI scope decision (2026-10-12)

The user does not use mobile. Current product UI support and acceptance are desktop-only; a narrow or resized desktop window remains desktop usage. Mobile browser support, mobile screenshots, and mobile viewport gates are out of scope. The 390px and 596px viewport targets below are retained as historical requirements for the archived A/B prototype, not as current production requirements. This scope decision does not grant a new SHIP disposition.

## Development-only direction contract

This contract guides the code-led preview; it is not part of the delivered HTML.

**THESIS** — Compare whether a first-time operator reaches a safe first read more clearly through one decision per step or through sections that expand in place.

**OWN-WORLD** — Extend Local Agent Harness’s incumbent interface: restrained light surfaces, Korean system typography, navy chrome, blue primary actions, and familiar form controls.

**STORY** — Start from the service URL the operator already has; make the resource candidate editable, explain missing authentication, review a narrow read scope, then show an explicitly synthetic first-read outcome.

**FIRST VIEWPORT** — Historical prototype requirement, superseded for current product acceptance on 2026-10-12: at 1440px, show the task and fixture controls, both equally weighted structure choices, and the selected structure’s URL input; at 596px and 390px, reflow controls and steps without horizontal scrolling.

**FORM** — Code-led static HTML/CSS/JavaScript. The user pinned the two structures and their shared task, so no generated comp or concept-seed round is needed. A uses four steps; B expands the same content inline.

**FINISH** — Historical boundary for the archived comparison: compare the prototype with the incumbent system, preserve product files, and report evidence checked. The production implementation was selected separately and is recorded in [`REGISTRATION_UI_IMPLEMENTATION.md`](REGISTRATION_UI_IMPLEMENTATION.md); this historical preview contract does not describe that implementation's scope.

## Latest decision and prototype boundary

The registration comparison below remains an archived/reference prototype of two competing **new connection** flows. Its A and B options keep the same task and fixtures so the original comparison remains fair. Prototype B is still progressive expansion for a new connection; it is not the production edit-existing-connection screen, and changing it into one would break the matched comparison.

The separate production UI decision is **guided A for adding a new connection** and **progressive review for editing an existing connection**. The A/B experiment remains a matched comparison of two new-connection prototypes; its B option is not the production edit screen. The production editor starts from existing values and keeps concise review guidance visible with vendor-specific instructions behind expandable details.

The production flow is implemented in [`ui.html`](../../ui.html): choose GitHub Enterprise, Jenkins, or Kubernetes Dashboard, check an HTTPS address through the local parser, review the candidate, and explicitly apply it. Then save ordinary fields as guided progress or use the separate manual save action. Credentials are never part of the saved draft. Resume clears typed credentials and old check permits; activation requires a current temporary connection check, a fixed-scope review, and explicit confirmation. A durable credential-storage/cleanup ambiguity is shown as a manual-recovery hold with no retry, delete, or activation control; ordinary `activation_pending` remains recoverable through a fresh check. After activation, the UI separately links to the existing service test, AI-app setup, a copy-only read prompt, and Readiness verification. URL suggestions remain shape-based and do not contact a service or prove that a repository, job, Dashboard route, credential, or AI-app read exists. The production client guide now covers manual ChatGPT and Claude Desktop setup as well as CLI guidance; Claude Desktop has a local preview MCPB, but actual installation, launch, and tool visibility remain unverified. The 2026-10-11 visual finish verdict covered the earlier URL preview/apply UI only; it does not validate the later guided lifecycle or recovery state. The updated implementation record describes the flow and validation boundary.

## 목적

[UX 조사](UX_RESEARCH_2026-10-10.md)와 [요구·결정 기록](UX_DECISIONS_2026-10-10.md)에서 정한 같은 등록 과제를 두 구조로 비교한다. 이 미리 보기의 목적은 시각 스타일을 바꾸는 것이 아니라, 첫 등록에서 화면을 나누는 방식과 한 화면에서 확장하는 방식을 검토하는 것이다.

보관된 preview 파일은 [`registration-options.html`](prototypes/registration-options.html)이다. A와 B는 같은 서비스, URL, 인증 fixture, AI 앱 선호, 범위 선택 및 첫 읽기 결과를 공유한다. 한 쪽에서 입력이나 시나리오를 바꾸면 다른 쪽에서도 같은 값으로 이어진다. 이 prototype의 성공·실패 결과와 권한 선택은 synthetic fixture이며 현재 제품 동작을 나타내지 않는다.

## 비교할 두 구조

| 안 | 화면 구성 | 기대 효과 | 확인할 비용 |
| --- | --- | --- | --- |
| A · 단계별 안내 | 주소 입력 → 인증 준비와 발급 안내 → 허용 작업 검토 → 첫 AI 읽기 예시. 한 번에 한 단계만 보이고 이전 단계로 돌아갈 수 있다. | 처음 등록할 때 토큰 발급·권한 확인 같은 화면 밖 작업에 집중하고, 진행 위치를 명확히 보여준다. | 단계가 많아 보이는지, 뒤로 가거나 초안을 이어갈 때 입력을 다시 요구하는지 확인한다. |
| B · 한 화면 순차 확장 | 주소 영역에서 시작해 인증, 범위, 첫 읽기 섹션을 같은 화면 아래로 차례로 펼친다. 섹션 제목을 열어 현재 정보도 다시 확인할 수 있다. | 현재 설정과 다음 내용을 함께 보고 편집하기 쉽다. | 화면이 길어질 때 다음 단계와 완료 상태를 놓치는지, 열린 섹션이 부담을 늘리는지 확인한다. |

두 안은 탭으로 전환하며 동등한 비중으로 제공한다. 순위나 승자를 미리 정하지 않는다. 이 비교는 연구 가설 H02를 화면 구조 수준에서 살펴보기 위한 설계물이며, 사용자 실험 결과나 통계적 우월성을 뜻하지 않는다.

## 공통 과제와 데이터

세 서비스 모두 HTTPS `*.invalid` 예시 주소와 합성 이름을 사용한다. URL을 붙여넣고 확인하면 선택된 서비스와 경로에서 대상 후보를 만든다. 미리 보기는 주소 형식만 확인하며 DNS 조회나 외부 요청을 보내지 않는다. query, fragment, 사용자 정보가 포함된 주소는 지우고 안전한 예시를 다시 사용하도록 안내한다.

주소 해석은 이 세 fixture를 위한 후보 제안이지 범용 URL 검증기가 아니다. GitHub는 처음 두 경로 구간, Jenkins는 경로의 `job/<segment>` 구간을 이름 후보로 사용한다. Jenkins 기준 fixture의 `Release-Gate`는 production-safe 세그먼트 문자 규칙에 맞춘다. 다른 Jenkins job 이름, 인코딩된 경로, context path, Dashboard 프록시 변형과 임의 서버 주소는 별도 호환성 검토가 필요하며 이 미리 보기에서 지원된다고 약속하지 않는다.

| 서비스 | 합성 주소 | 첫 읽기 과제 | 읽기 범위 | 꺼진 추가 권한 |
| --- | --- | --- | --- | --- |
| GitHub Enterprise | `https://git.enterprise.invalid/platform/observability` | 저장소 기본 정보와 기본 브랜치 보기 | 지정 저장소의 기본 정보와 브랜치 읽기 | 파일 콘텐츠 읽기 |
| Jenkins | `https://ci.invalid/jenkins/job/Platform/job/Release-Gate` | job 상태와 최근 빌드 보기 | 선택한 job 및 최근 빌드 읽기 | 빌드 실행 |
| Kubernetes Dashboard · prototype용 synthetic fixture | `https://dashboard.invalid/k8s/dashboard` | `platform/edge-api` Deployment 상태 보기 | 지정 namespace와 Deployment 읽기 | 모든 namespace 목록 읽기 |

읽기 권한은 prototype 과제에 맞춘 synthetic 선택지다. 실행·쓰기 또는 넓은 목록 범위는 기본 선택하지 않는다. 이 비교 페이지의 허용 범위 컨트롤은 실제 제품 설정을 바꾸지 않는다. production UI는 현재 실제 저장·운영 동작만 안내하며 prototype 선택지를 노출하지 않는다.

## 인증과 실패 시나리오

공통 fixture 선택은 두 구조에서 같은 결과를 낸다.

- **인증 준비됨 · 합성 성공:** 600ms 이하의 화면 내 대기 후 고정된 예시 값을 보여준다. “합성 데모 성공”을 표시하고 실제 연결 확인이 아님을 다시 설명한다.
- **토큰 없음 · 발급 안내:** 비밀값 입력 없이 발급 위치와 리소스별 읽기 권한을 안내한다. 첫 읽기는 중단 결과를 표시한다. 일반 설정만 탭 메모리에 저장한 초안을 이어갈 수 있다.
- **권한 부족 · 합성 실패:** 선택한 리소스에 대한 합성 실패와 복구 안내를 표시한다. Jenkins의 403만으로 인증 오류와 권한 부족을 구분할 수 없음을 알린다.

초안에는 서비스, 합성 URL, 연결 이름, 일반 읽기 범위, 앱 선호만 둔다. 초안은 브라우저 탭 메모리에서만 유지되며 새로고침이나 탭 종료 후에는 없어지는 미리 보기다. 비밀값 필드는 없고 토큰을 저장하거나 재노출하지 않는다.

## 인증 및 연결 문구의 경계

- GitHub Enterprise는 서버 버전, 토큰 종류, 지정 저장소의 최소 읽기 권한, 조직 승인 상태를 확인하도록 안내한다. 예시 서버 버전의 메뉴와 권한을 미확인 서버에 보장하지 않는다.
- Jenkins는 계정/API 토큰 발급 위치, context path와 폴더 job 경로, job 읽기 권한을 안내한다. 빌드 실행은 첫 읽기 과제에 포함하지 않는다.
- Dashboard prototype fixture는 설치된 제품 버전이나 인증 동작을 검증하지 않는다. 사용자가 화면에서 본 `1.7.0` 표시는 실제 설치 구성 요소·버전을 특정하지 못한다. 현재 제품 UI는 저장한 값을 `Authorization: Bearer`로 보내며 로그인이나 토큰 교환을 하지 않는다. 설치 문서나 관리자와 실제로 필요한 자격 증명을 확인하도록 안내하고 `/api/v1/login` 응답을 곧바로 복사 가능한 값이라고 취급하지 않는다. 상세 계약은 [Dashboard 1.7 구현 계약](DASHBOARD_1_7_IMPLEMENTATION_CONTRACT.md)을 참고한다.
- AI 앱 선호는 ChatGPT Desktop, Claude Desktop, 현재 CLI 대체 경로인 Codex CLI 중 고른다. Desktop 연결·설치·호출이 검증되었다고 표시하지 않는다. Codex CLI를 선택해도 미리 보기에서 실제 CLI를 실행하지 않는다.

## 역사적 prototype 시각·접근성 기준

이 기준은 보관된 비교 prototype을 위한 것이다. 기존 `ui.html`과 같은 밝은 표면, 시스템 한국어 글꼴, 남색 상단 바, 파란 기본 동작, 익숙한 폼 컨트롤을 이어간다. 새 브랜드나 제품 내비게이션을 만들지 않는다. 44px 이상의 입력·버튼, 연결된 레이블, 키보드 탭 전환, 포커스 표시, 진행 단계, 필드 가까운 오류, 결과의 보조기기 공지를 둔다. 당시에는 1440px, 596px, 390px를 비교 범위로 적었지만, 2026-10-12 결정에 따라 이 폭들은 현재 제품 acceptance gate가 아니다. 현재 UI는 desktop-only로 평가하며, 좁은 창도 데스크톱 창으로 취급한다.

## 데모 한계와 다음 확인

미리 보기에는 `fetch`, 외부 브라우저 이동, 실제 토큰 입력, 앱 설정 변경이 없다. 결과·권한·Desktop 상태는 합성 fixture다. 실제 서비스 계약, Dashboard 설치 구성 요소와 프록시 인증, Desktop 설치 경로는 이 산출물에서 결정하지 않는다.

사람에게 보여줄 때는 서비스 하나와 fixture 하나를 고르고 A와 B에서 같은 과제를 수행하게 한다. URL 이해, 토큰이 없을 때 다음 행동 찾기, 범위 검토, 뒤로가기 후 입력 보존, 실패 후 복구, 데모와 실제 연결의 구분을 관찰한다. 외부 승인·VPN 대기 시간과 화면 내 작업을 분리한다. 관찰 전에는 A 또는 B를 기본안으로 확정하지 않는다.
