# Dashboard 1.7.0: 인증 안내와 호환성 확인

확인일: 2026-10-10. 상태: **upstream 소스와 현재 앱 비교, 수정·실제 설치 검증 미완료**.

사용자 보고: 화면에 표시된 Dashboard 버전은 1.7.0이며 토큰을 붙여넣는 로그인 화면을 사용한다. 실제 사내 주소·프록시 설정·토큰은 수집하지 않았다. [UX 조사](UX_RESEARCH_2026-10-10.md)의 등록·인증 흐름에 영향을 주는 구체적인 기술 발견을 기록한다.

**버전 식별 정정:** 공식 릴리즈에는 구형 전체 버전 [v1.7.0](https://github.com/kubernetes-retired/dashboard/releases/tag/v1.7.0)과 별도 웹 컴포넌트 [web/v1.7.0](https://github.com/kubernetes-retired/dashboard/releases/tag/web%2Fv1.7.0)이 함께 존재한다. 화면의 숫자와 토큰 로그인만으로 실제 API/auth backend를 식별할 수 없다. 아래 소스 비교는 구형 전체 태그에만 해당한다. 실제 설치의 chart/release와 API/auth 이미지 또는 단일 서버 이미지를 확인하기 전에는 인증 방식을 추정하거나 변경하지 않는다. 구체적인 식별·구현 계약은 [추가 계약](DASHBOARD_1_7_IMPLEMENTATION_CONTRACT.md)을 따른다.

## 발견

현재 앱은 Dashboard 로그인에서 얻은 API Bearer를 입력하라고 안내하고, 저장값을 `Authorization: Bearer <value>`로 전송한다. 위치: [입력 안내](../../ui.html), [Dashboard 구현](../../dashboard.go)의 `loadDashboardBearer`와 원격 요청 함수. 합성 테스트도 이 전송 계약을 기준으로 한다.

upstream v1.7.0에서는 로그인 결과와 Kubernetes API 인증 토큰이 서로 다르다.

| 입력·처리 | v1.7.0 소스에서 확인한 계약 |
| --- | --- |
| 로그인 입력 | `LoginSpec.token`은 Kubernetes 인증용 bearer token |
| 로그인 결과 | `AuthResponse.jweToken` 반환, 오류 목록도 포함 |
| API 요청의 `Authorization: Bearer` | 원문을 Kubernetes `AuthInfo.Token`으로 사용 |
| API 요청의 `jweToken` 헤더 | Dashboard token manager로 복호화 |
| 두 헤더가 함께 있을 때 | `Authorization: Bearer`가 우선 |
| Dashboard JWE의 기본 유효기간 | 900초, 설정으로 변경 가능 |

근거: [client manager의 인증 분기](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/client/manager.go#L226), [인증 타입](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/auth/api/types.go), [로그인 handler](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/auth/handler.go).

**구형 태그 소스 비교에 따른 결론:** 로그인 결과 JWE를 Bearer 헤더로 보내는 현재 안내는 수정되지 않은 구형 전체 upstream v1.7.0 계약과 맞지 않는다. 사용자의 설치가 이 태그에 해당하는지, 사내 프록시가 값을 변환하는지, 배포가 수정됐는지는 미확인이다. 이는 구형 태그의 호환 위험 근거이며 실제 설치에서 실패를 재현한 결과는 아니다.

## UX에 필요한 변화

먼저 화면 표시 버전과 실제 backend 버전을 구분한다. 구형 전체 태그로 확인된 경우에는 **로그인 화면에 넣는 Kubernetes 토큰**과 **로그인 후 Dashboard가 발급한 JWE**를 구분한다. 기본 화면에서 HTTP 헤더를 직접 선택하게 만들기보다는 확인된 설치 계약에 맞는 연결 방법을 안내한다.

선택 가능한 구현 방향은 다음과 같다. 아직 어느 방식도 추가 구현하지 않았다.

| 방향 | 장점 | 필요한 확인 |
| --- | --- | --- |
| 로그인 입력 토큰을 저장해 Dashboard의 Bearer 경로로 조회 | upstream의 해당 분기와 직접 맞음, 별도 JWE 수동 복사 감소 | 현재 ‘raw Kubernetes 토큰 미지원’ 제품 안내·정책을 명시적으로 변경해야 함. 사내 프록시가 헤더를 전달하는지 확인 |
| 앱이 Dashboard 로그인 흐름을 수행하고 JWE로 조회 | 사용자가 익숙한 로그인용 토큰을 입력, 세션 처리 자동화 가능 | 로그인 POST·CSRF·오류 응답·세션 만료·갱신 계약, 비밀 저장 범위, 네트워크 제한을 별도 구현·검증 |
| 이미 발급된 JWE를 별도 모드로 입력 | 로그인 자동화 없이 계약 구분 가능 | 짧은 세션 만료 때문에 반복 입력이 생길 수 있음. 기본 등록 방식으로 적합한지 사용자 검증 |

UI에서 로그인 과정을 안내하는 편의와 기존 서버 정책을 동시에 검토한다. 버전 문자열만으로 인증 모드를 조용히 바꾸거나 실패 뒤 다른 토큰 방식으로 자동 재시도하지 않는다. 자격 증명 원문은 AI 대화·MCP 인자·일반 설정 초안으로 이동시키지 않는다.

## 경로 확인 범위

[v1.7.0 API handler](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/handler/apihandler.go)는 `/api/v1` 아래 namespace·Deployment·ReplicaSet·Pod·log 관련 경로를 정의한다. 현재 앱도 저장한 기본 주소 아래 `/api/v1/` 경로를 조립한다.

경로 이름이 존재하는 것과 반환 구조·프록시 prefix·권한·페이지네이션까지 호환되는 것은 다르다. 현재 합성 fixture를 ‘공식 1.7.0 호환 검사’라고 바꾸기 전에 각각 대조한다. SPA의 URL fragment와 프록시 prefix를 혼동해 저장 주소를 잘못 잘라내지 않아야 한다.

## 실제 대상 없이 준비할 테스트 계획

- [ ] Bearer와 JWE를 구분하는 공식 분기 기반 fixture를 만든다.
- [ ] 잘못된 토큰 종류를 넣었을 때 안전한 오류와 복구 안내를 제공한다.
- [ ] JWE 만료·로그인 오류 목록·비정상 응답·프록시 로그인 HTML을 다룬다.
- [ ] 인증용 POST를 지원할 경우 CSRF와 로그인 전용 범위만 구현한다. 배포·수정 API로 확대하지 않는다.
- [ ] namespace 목록 권한과 지정 Deployment 읽기 권한을 구분한다.
- [ ] 현재 Deployment·events·ReplicaSet Pods·logs의 응답 필드와 범위를 대조한다.
- [ ] 프록시 prefix·redirect·다른 origin·TLS 실패를 검증하고 인증 정보가 다른 대상에 전달되지 않게 한다.
- [ ] 설정을 바꾸면 기존 연결 확인·첫 조회 상태가 현재 근거로 재사용되지 않게 한다.
- [ ] 합성 검사 성공과 실제 사내 설치 성공을 별도로 기록한다.

다음 실제 검증에서는 먼저 설치 track과 API/auth backend를 식별하고, 로그인 입력 토큰 종류, proxy의 인증 전달, TLS, 제한된 읽기 권한을 확인한다. 토큰 원문이나 전체 kubeconfig를 조사 문서·공개 로그에 제출할 필요는 없다.
