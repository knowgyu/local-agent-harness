# UX 근거 목록과 읽은 범위

모든 자료의 확인일은 2026-10-10이다. 아래 날짜는 페이지에서 확인한 발표·검토일 또는 논문 연도다. 날짜가 없는 최신 문서를 ‘2026년에 발표된 연구’로 표현하지 않는다. 검색 결과의 수집일도 발표일로 사용하지 않는다.

**읽은 범위**의 ‘관련 본문’은 해당 주제의 절을 확인했다는 뜻이다. 논문의 모든 실험·통계·부록을 재검증했다는 뜻은 아니다. 제품 공식 문서는 기능과 안내 경로의 근거이고, 해당 UX의 비교 우월성에 대한 실험 근거는 아니다. 모든 요약은 자체 문장으로 작성했다.

[적용 제안](UX_RESEARCH_2026-10-10.md) · [요구 기록](UX_DECISIONS_2026-10-10.md) · [Dashboard 호환성](DASHBOARD_1_7_COMPATIBILITY.md)

## 제품 사례

### P01. Claude Desktop Extensions

- 출처: [Anthropic Engineering](https://www.anthropic.com/engineering/desktop-extensions)
- 날짜·범위: 2025-06-26 발표, 2025-09-11 명칭 변경 안내. 설치·manifest·필수 설정·비밀 저장 관련 본문 확인.
- 관찰: 배포와 사용자 설정을 패키지 경험으로 묶는다. 필요한 입력을 선언하고 민감 값은 OS 비밀 저장소에 보관한다.
- 적용·한계: AI 앱 연결의 수동 파일 작업을 줄이는 참고. 이 앱의 binary 패키지·업데이트·실제 Desktop 동작은 별도 검증한다.

### P02. VS Code MCP 관리

- 출처: [VS Code 공식 문서](https://code.visualstudio.com/docs/agent-customization/mcp-servers)
- 날짜·범위: 발표일 미표시, 조사 시점 현행 문서. Quickstart·설치·guided flow·신뢰 관련 본문 확인.
- 관찰: 설치 후 도구 발견과 채팅 요청까지 안내한다. 자동 설치와 수동 구성 모두 제공한다.
- 적용·한계: 클라이언트 등록 다음에 실제 과제를 제공한다. VS Code 기능이나 플랫폼별 격리를 이 앱의 기능으로 전용하지 않는다.

### P03. Docker MCP Toolkit

- 출처: [Docker 공식 문서](https://docs.docker.com/ai/mcp-catalog-and-toolkit/toolkit/)
- 날짜·범위: 발표일 미표시. 본문은 Docker Desktop 4.62 이후 UI와 Beta 상태를 명시. 인증·클라이언트 연결 절 확인.
- 관찰: 서비스 선택 뒤 지원 서버에 대해 브라우저 인증을 진행하고 클라이언트를 연결한다.
- 적용·한계: 인증을 등록 맥락에 배치한다. 모든 서비스에 OAuth가 가능하거나 이 앱에 Docker 수준의 격리가 있다는 뜻은 아니다.

### P04. Docker MCP Profiles

- 출처: [Docker 프로필 문서](https://docs.docker.com/ai/mcp-catalog-and-toolkit/profiles/)
- 날짜·범위: 발표일 미표시. 서버 구성·OAuth 자격 증명·공유·클라이언트 연결 절 확인.
- 관찰: 구성 템플릿과 인증을 구분한다. OAuth 자격 증명은 모든 프로필에서 공유되고, 공유 프로필에는 자격 증명을 포함하지 않는다.
- 적용·한계: 복제와 템플릿은 유용한 후보지만 그룹을 인증 계정 격리로 설명하지 않는다. 1~5개 사용에서 프로필 관리의 필요성은 아직 확인하지 않았다.

### P05. n8n Credentials

- 출처: [n8n 자격 증명 생성 문서](https://docs.n8n.io/build/understand-workflows/create-and-edit-credentials.md)
- 날짜·범위: 발표일 미표시. 서비스별 입력·문서 연결·저장 시 테스트 관련 본문 확인.
- 관찰: 인증 요구에 맞는 입력과 서비스별 도움말을 제공한다. 저장 시 인증 검사를 수행한다.
- 적용·한계: 발급 안내와 검사를 연결한다. 자격 증명 유효성, 특정 리소스 접근, AI 앱의 실제 사용은 각각 확인해야 한다.

### P06. Postman Import

- 출처: [Postman 공식 가져오기 문서](https://learning.postman.com/docs/getting-started/importing-and-exporting/importing-data)
- 날짜·범위: 발표일 미표시, 페이지 표시 Latest v12. URL·텍스트·cURL·저장하지 않는 가져오기 절 확인.
- 관찰: 기존에 가진 자료를 편집 가능한 요청으로 옮길 수 있다.
- 적용·한계: URL을 먼저 받는 설계의 참고. cURL 실행이나 임의 API 권한 허용을 제안하는 근거로 사용하지 않는다.

### P07. Grafana TestData

- 출처: [Grafana 공식 문서](https://grafana.com/docs/grafana/latest/datasources/testdata/)
- 날짜·범위: Last reviewed 2026-08-11. 외부 연결 없는 합성 데이터·체험·재현 관련 본문 확인. Markdown 대체 URL은 조회 실패하여 HTML을 사용.
- 관찰: 내장 합성 데이터로 화면을 만들고 문제를 재현할 수 있다.
- 적용·한계: 실제 대상이 없는 동안 독립된 데모와 오류 fixture를 준비한다. 합성 성공은 사내 설치 호환성의 증거가 아니다.

### P08. Tailscale Add device

- 출처: [Tailscale 공식 문서](https://tailscale.com/docs/features/access-control/device-management/how-to/set-up)
- 날짜·범위: Last validated 2026-01-05. 설치·인증·관리자 승인 관련 본문 확인.
- 관찰: 기기 추가와 승인 대기를 다른 상태로 보여준다.
- 적용·한계: 인증 완료와 사용 가능 상태를 구분하고 대기 중 복귀 경로를 준비한다. 네트워크 설치 절차 자체는 이 앱 범위가 아니다.

### P09. 기업용 Desktop Extensions, 2026

- 출처: [Anthropic 기업 배포 안내](https://support.claude.com/en/articles/12702546-deploying-enterprise-grade-mcp-servers-with-desktop-extensions)
- 날짜·범위: 2026-08-05. 로컬 접근과 관리자 제어 관련 본문 확인.
- 관찰: 사내 리소스에 대한 로컬 확장 경로와 조직 배포를 설명한다.
- 적용·한계: Desktop 설치를 별도 UX로 설계할 근거. 페이지의 일반적 SSO 편의 설명만으로 이 앱에 자동 세션 재사용이 생기지는 않는다.

### P10. Claude Desktop 로컬 MCP

- 출처: [Claude 공식 지원 문서](https://support.claude.com/en/articles/10949351-getting-started-with-local-mcp-servers-on-claude-desktop)
- 날짜·범위: 정확한 발표일 미표시. 로컬 확장·custom MCPB·binary 지원·연결 확인 관련 본문 확인.
- 관찰: 설정 화면에서 패키지를 설치하고 필요한 구성을 채운다. 조직 정책이 설치에 영향을 줄 수 있다.
- 적용·한계: Claude Desktop을 Claude Code CLI와 구분한다. 이 저장소의 검증된 Desktop 배포물은 아직 없다.

### P11. ChatGPT Desktop·Codex MCP

- 출처: [OpenAI 현행 MCP 문서](https://learn.chatgpt.com/docs/extend/mcp?surface=cli) — 기존 developers.openai.com/codex/mcp에서 연결됨.
- 날짜·범위: 발표일 미표시. 지원 기능·동일 host의 설정 공유·stdio 구성 관련 본문 확인.
- 관찰: ChatGPT Desktop·Codex CLI·IDE 확장이 같은 Codex host에서 설정을 공유한다고 설명한다.
- 적용·한계: 현재 CLI 등록을 Desktop 안내에 재사용할 후보. 특정 설치 버전·프로필·대화에서 실제 도구 호출이 성공했다는 증거는 아니다.

## 폼·접근성·AI 상호작용 가이드

### C01. Progressive Disclosure

- 출처: [Jakob Nielsen, NN/G](https://www.nngroup.com/articles/progressive-disclosure/)
- 날짜·범위: 2006-12-03. 기본·고급 기능 분리와 발견 가능성 관련 본문 확인. 전문가 가이드.
- 적용: 주요 행동은 기본 화면에, 드문 설정은 분명한 추가 경로에 둔다. 핵심 권한과 제약은 숨기지 않는다.

### C02. Structuring forms

- 출처: [GOV.UK Service Manual](https://www.gov.uk/service-manual/design/form-structure)
- 날짜·범위: 2016-12-07 발표, 2018-08-07 갱신. 질문 필요성·분기·페이지 병합 관련 본문 확인. 서비스 설계 가이드.
- 적용: 각 입력의 목적을 정의하고 관련 질문만 노출한다. 반복 업무에서는 한 결정 한 페이지를 기계적으로 적용하지 않는다.

### C03. Check answers

- 출처: [GOV.UK Design System](https://design-system.service.gov.uk/patterns/check-answers/)
- 날짜·범위: 발표일 미확정. 확인 화면·수정 링크·값 유지 관련 본문 확인.
- 적용: 저장 직전 리소스와 허용 작업을 짧게 검토한다. 전체 민감 입력을 그대로 다시 표시하지 않는다.

### C04. Error message

- 출처: [GOV.UK Design System](https://design-system.service.gov.uk/components/error-message/)
- 날짜·범위: 변경 목록에 2024-01 입력 보존 안내 포함. 필드 연결·구체적 문구·값 보존 관련 본문 확인.
- 적용: 사용자가 수정할 수 있는 문제와 다음 행동을 구체적으로 설명한다. 서비스 장애·권한 실패를 필드 형식 오류와 혼동하지 않는다.

### C05. Multi-page Forms

- 출처: [W3C WAI](https://www.w3.org/WAI/tutorials/forms/multi-page/)
- 날짜·범위: 2019-07-27 갱신. 논리 단계·진행·선택 단계·복귀 관련 본문 확인. 접근성 구현 안내.
- 적용: 단계 수와 제목·현재 위치를 알리고 뒤로 이동해도 일반 입력을 보존한다. 단계가 많은 것이 곧 접근성이 좋은 것은 아니다.

### C06. Redundant Entry

- 출처: [W3C WCAG 2.2, SC 3.3.7 해설](https://www.w3.org/WAI/WCAG22/Understanding/redundant-entry.html)
- 날짜·범위: 현행 Understanding 문서, 발표일 미확정. 기준·목적·예외 본문 확인. 기준을 설명하는 비규범 문서.
- 적용: 같은 과정의 일반 입력을 다시 묻지 않는다. 필수·보안·만료 예외가 있으며 토큰 원문 보관의 근거로 쓰지 않는다.

### C07. Accessible Authentication

- 출처: [W3C WCAG 2.2, SC 3.3.8 해설](https://www.w3.org/WAI/WCAG22/Understanding/accessible-authentication-minimum)
- 날짜·범위: 현행 Understanding 문서, 발표일 미확정. 붙여넣기·password manager·인증 단계 관련 본문 확인.
- 적용: 인증 입력을 외워서 타이핑하게 만들지 않는다. 여기서는 토큰 입력 설계의 참고이며 전체 준수 판정은 하지 않았다.

### C08. Guidelines for Human-AI Interaction

- 출처: [Amershi 외, Microsoft Research 저자 PDF](https://www.microsoft.com/en-us/research/wp-content/uploads/2019/01/Guidelines-for-Human-AI-Interaction-camera-ready.pdf), [HAX Toolkit](https://www.microsoft.com/en-us/haxtoolkit/ai-guidelines/)
- 날짜·범위: CHI 2019. 평가 절차·기대 설정·수정·논의 관련 본문 확인. Toolkit 페이지에서 모든 가이드 문구가 추출되지 않아 논문을 확인.
- 관찰: HCI 실무자 평가 등을 거쳐 18개 가이드를 다듬었다.
- 적용·한계: URL 추론의 한계를 알리고 수정·복구를 쉽게 한다. 특정 화면안의 성능을 비교한 연구로 해석하지 않는다.

## 실험 연구와 2026 논의

### R01. Designing usable web forms

- 출처: [Seckler 외, University of Copenhagen 연구 포털](https://researchprofiles.ku.dk/en/publications/designing-usable-web-forms-empirical-evaluation-of-web-form-impro/)
- 날짜·범위: CHI 2014, DOI 10.1145/2556288.2557265. **기관 초록과 출판 정보만 확인**. Google 호스팅 PDF 조회 실패.
- 관찰: N=65, 20개 가이드를 결합한 폼 개선의 효과를 보고한다.
- 적용·한계: 폼 구조·오류·재입력을 함께 개선하는 근거. 단일 가이드의 효과나 이 앱의 시간 단축 비율은 알 수 없다.

### R02. To Trust or to Think

- 출처: [Buçinca 외, 저자 PDF](https://www.eecs.harvard.edu/~kgajos/papers/2021/bucinca21trust.pdf)
- 날짜·범위: CSCW1 2021-04, DOI 10.1145/3449287. 초록·실험 과제·조건·주요 결과 관련 본문 확인.
- 관찰: N=199, 영양 선택 과제와 모의 AI. 인지적 개입의 과신 감소와 주관적 사용성 비용을 다룬다.
- 적용·한계: 검토를 무조건 없애거나 확인을 계속 늘리지 않는다. 등록·토큰 발급 과제의 직접 실험은 아니다.

### R03. Designing for Effective Transparency

- 출처: [Springer·Whittaker, arXiv](https://arxiv.org/abs/1811.02164)
- 날짜·범위: 2018-11-06 제출. **초록만 확인**. 이 조사에서 출판 심사 상태·전체 실험 설계는 확인하지 않았다.
- 관찰: 두 연구에서 점진적인 세부 피드백의 예상 가치와 실제 경험이 달라질 수 있음을 논의한다.
- 적용·한계: 원시 내부 로그의 기본 노출을 줄이는 가설. 오류를 숨기는 근거로 사용하지 않는다.

### R04. Cognitive Forcing for Execution Plans, 2026

- 출처: [Ghosh 외, arXiv HTML](https://arxiv.org/html/2601.18033v1), [서지·초록](https://arxiv.org/abs/2601.18033)
- 날짜·범위: 2026-01-25 제출 v1. 방법·참여자·논의·한계 절 확인. 사전 공개 논문, 심사 완료 여부 미확인.
- 관찰: 214명의 비교 실험과 후속 인터뷰를 통해 작성 계획의 가정 검토와 다른 개입을 비교했다. 사전 고정 AI 결과와 합성 오류를 사용했다.
- 적용·한계: 집중된 범위 검토와 과도한 반복 질문을 비교할 참고. 기술 설정·한국어·사내 권한 환경으로 일반화하지 않는다.

### R05. AI Agents as Users, 2026

- 출처: [Sarah Gibbons·Kate Moran, NN/G](https://www.nngroup.com/articles/ai-agents-as-users/)
- 날짜·범위: 2026-04-10. 접근성·의미 구조·인간과 에이전트의 상호작용 관련 본문 확인. 전문가 논의.
- 적용·한계: 명확한 이름·역할·상태를 가진 컨트롤을 제공한다. 사용자 권한 판단이나 인증 입력을 에이전트에 위임하는 근거는 아니다.

## 서비스별 인증·호환성

### A01. GitHub Enterprise Server PAT

- 출처: [GitHub Enterprise Server 3.22 공식 문서](https://docs.github.com/en/enterprise-server@3.22/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
- 날짜·범위: latest 경로가 조사 시점 3.22로 연결됨. 토큰 유형·제한·최소 접근·조직 승인·endpoint별 권한 관련 본문 확인.
- 적용·한계: 버전별 발급 안내와 권한 범위를 준비한다. 사용자의 실제 서버 버전·정책은 미확인이다. Enterprise Cloud 안내와 혼용하지 않는다.

### A02. Jenkins Remote API

- 출처: [Jenkins 공식 문서](https://www.jenkins.io/doc/book/using/remote-access-api/)
- 날짜·범위: 발표일 미표시. 리소스별 API 위치·조회와 실행 구분 관련 본문 확인.
- 적용·한계: 작업 URL을 기본 주소와 리소스로 해석하는 참고. 실제 서버 context path와 보안 설정은 확인이 필요하다.

### A03. Jenkins API 인증

- 출처: [Jenkins scripted clients 인증 문서](https://www.jenkins.io/doc/book/system-administration/authenticating-scripted-clients/)
- 날짜·범위: 발표일 미표시. 계정/API 토큰·발급 위치·preemptive authentication·403 설명 확인.
- 적용·한계: 토큰 발급 도움과 오류 안내에 사용. 403 하나로 권한 부족을 확정할 수 없다. 사용자 서버에서의 발급 메뉴는 별도 확인한다.

### A04. Kubernetes Dashboard v1.7.0 소스

- 출처: [인증 헤더 처리](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/client/manager.go), [로그인 endpoint](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/auth/handler.go), [토큰 형식과 기본 TTL](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/auth/api/types.go), [API 경로](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/handler/apihandler.go)
- 날짜·범위: 구형 전체 버전 고정 태그 v1.7.0. 해당 함수·타입·경로 본문 확인. 배포·인증 코드를 실행하지 않았다. 화면 표시 1.7.0을 이 전체 태그로 매핑할 근거는 없다(A05 참조).
- 관찰: `Authorization: Bearer`와 `jweToken` 헤더를 서로 다른 인증 입력으로 처리한다.
- 적용·한계: 현재 안내와 전송 방식의 불일치를 등록 개선 전에 해결한다. 사내 프록시 변환·수정 배포·실제 권한은 소스로 알 수 없다.

### A05. 전체 버전과 web 컴포넌트 버전의 식별

- 출처: [전체 v1.7.0 릴리즈](https://github.com/kubernetes-retired/dashboard/releases/tag/v1.7.0), [web/v1.7.0 릴리즈](https://github.com/kubernetes-retired/dashboard/releases/tag/web%2Fv1.7.0)
- 날짜·범위: 2026-10-10에 두 공식 릴리즈의 태그·이미지 이름을 확인했다. 전체 태그는 `kubernetes-dashboard-amd64:v1.7.0`, 웹 컴포넌트는 `kubernetesui/dashboard-web:1.7.0` 이미지다.
- 관찰: 같은 1.7.0 숫자가 서로 다른 배포 구성 요소를 가리킬 수 있다. 사용자는 화면 표시에서 버전을 확인했다고 답했다.
- 적용·한계: 화면 표시와 토큰 로그인만으로 backend 인증 계약을 확정하지 않는다. 실제 chart/release 및 API/auth 이미지 또는 단일 서버 이미지를 먼저 식별한다.

## 사용하지 않은 자료와 남은 조사

검색 과정의 커뮤니티 글, 제3자 제품 소개, 인증 관련 최신 논문 일부는 설계 주장 근거로 채택하지 않았다. API 프로토콜 최신화 자체도 이번 UX 조사 범위로 확대하지 않았다.

실제 설치 대상이 확보되면 서비스 버전·프록시·권한 정책과 Desktop별 활성 세션의 첫 조회를 확인한다. 그 전에는 위 자료로 설계와 합성 검증을 준비하며 실제 성공을 주장하지 않는다.
