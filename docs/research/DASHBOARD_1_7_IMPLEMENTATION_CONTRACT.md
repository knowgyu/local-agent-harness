# Dashboard 1.7.0 구현 계약과 근거 검토

작성일: 2026-10-10. 목적: Dashboard 인증과 첫 조회를 고칠 때 지켜야 할 호환성·저장·검증 계약을 정한다. 이 문서는 설계안이며 구현이나 실제 설치 검증을 뜻하지 않는다.

## 검증 상태와 버전 경계

사용자가 정정한 버전 출처는 Dashboard 화면에 표시된 `1.7.0`이다. 로그인 화면에 토큰을 붙여넣는다는 점도 사용자 보고다. 실제 주소, 이미지 태그, chart 버전, API 컨테이너, 수정 여부, 로그인 응답, 권한은 확인하지 않았다.

공식 저장소의 `v1.7.0` 태그는 SHA `cddd14383a9fcfee80451251e738f0b29ac59d66`, 2017-09-22의 레거시 단일 서버 소스다. 같은 공식 저장소에는 2025-05-28의 별도 컴포넌트 릴리스 `web/v1.7.0`도 있다. 그러므로 “Dashboard 1.7.0”만으로 현재 설치의 API 서버 계약을 식별할 수 없다. 아래 인증 및 응답 비교는 **2017년 `v1.7.0` 태그에 한정해 소스에서 확인한 사실**이다. 2025년 웹 컴포넌트나 사내 fork에 그대로 적용된다고 말할 근거는 없다. [레거시 `v1.7.0` 소스 태그](https://github.com/kubernetes-retired/dashboard/tree/v1.7.0), [공식 `web/v1.7.0` 컴포넌트 릴리스](https://github.com/kubernetes-retired/dashboard/releases/tag/web/v1.7.0)

### 화면 버전에서 시작하는 최소 식별 절차

우선 기록할 값은 `UI 표시 버전=1.7.0`이며, 이는 아직 backend 버전이 아니다. 설치 관리자나 read-only 배포 정보에서 같은 Dashboard 설치의 Helm chart/release 버전과 API/auth 서버 컨테이너의 image 이름·tag를 확인한다. 이미지 tag가 사내에서 재사용되거나 덮어쓸 수 있으면 digest도 함께 확인한다. 단일 이미지 설치라면 해당 서버 이미지 tag/digest를 확인한다. `web` 이미지의 `1.7.0`만 확인되면 API 계약은 계속 미식별 상태로 둔다. 이 값을 확인할 수 없으면 실제 backend 동작을 주장하거나 버전에 따라 인증을 자동 선택하지 않는다. URL, 토큰, kubeconfig는 이 식별에 필요하지 않다.

## 인증 계약

레거시 태그의 `LoginSpec.token`은 Kubernetes API 인증에 쓰는 원본 Bearer이고, `/api/v1/login` 응답의 `jweToken`은 그 인증 정보를 담아 Dashboard가 만든 JWE다. `manager.go`는 `Authorization: Bearer`가 있으면 그 값을 그대로 Kubernetes `AuthInfo.Token`에 넣는다. Bearer가 없고 `jweToken` 헤더가 있으면 Dashboard token manager가 복호화한다. 두 값이 동시에 있으면 Bearer 분기가 우선한다. 기본 JWE 유효기간은 900초이며 설정으로 바꿀 수 있다. [로그인 타입과 TTL](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/auth/api/types.go#L28-L38), [로그인 입력·응답](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/auth/api/types.go#L116-L138), [인증 헤더 분기](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/client/manager.go#L246-L278)

현재 [ui.html](../../ui.html#L607-L610)은 `/api/v1/login`에서 받은 “API Bearer”를 넣으라고 안내하고 원본 Kubernetes 토큰은 지원하지 않는다고 말한다. [dashboard.go](../../dashboard.go#L1171-L1178)의 원격 요청은 저장값을 `Authorization: Bearer`로 보낸다. 레거시 upstream과 비교하면 로그인 응답 JWE를 원본 Bearer처럼 전송하는 계약 불일치다. 이는 소스 비교 결과이며 사용자의 설치에서 실패를 재현한 것은 아니다.

### 최소 인증 변경안

레거시 API 계약을 대상으로 새 연결을 지원할 때는 로그인 화면에 붙여넣는 **Kubernetes API Bearer**를 저장하고 `Authorization: Bearer <값>`으로 보낸다. 앱이 `/api/v1/login`을 대신 호출하거나 토큰을 교환하지 않는다. 대상의 정확한 API 서버 버전이 확인되기 전에는 이를 “Dashboard 1.7.0 전반”에 적용된 기본값이라고 표시하지 않는다.

저장 대상에 선택적인 `auth_mode`를 둔다. 최소 첫 변경에서 값은 `legacy_bearer`와 `kubernetes_bearer`로 제한한다.

| 모드 | 적용 | 전송·입력 계약 |
| --- | --- | --- |
| `legacy_bearer` | 기존 설정에서 필드가 없을 때의 유효 기본값 | 현재처럼 저장된 바이트를 Bearer로 전송한다. 입력값의 종류를 추정하거나 바꾸지 않는다. |
| `kubernetes_bearer` | 새 대상 또는 사용자가 명시적으로 바꾼 대상 | Dashboard 로그인 화면에 넣는 Kubernetes API Bearer를 새로 입력하게 하고 Bearer로 전송한다. 로그인 결과 JWE를 뜻하지 않는다. |

기존 대상은 `legacy_bearer`로 계속 동작한다. 수정 화면에서 모드를 `kubernetes_bearer`로 바꿀 때는 새 자격 증명을 다시 입력하게 한다. 빈 입력은 기존 모드와 Credential Manager 참조를 보존한다. 모드나 URL이 바뀌면 기존 연결 테스트 결과를 재사용하지 않는다. 등록·MCP 도구에 임의 헤더, 임의 endpoint 또는 인증 인자를 추가하지 않는다.

JWE 직접 입력은 첫 수정 범위에 넣지 않는 편이 안전하다. 필요가 확인되면 별도 `dashboard_jwe` 모드로만 추가하고, 공식 `jweToken` 헤더 하나만 보내며, 만료를 명시적으로 다뤄야 한다. Bearer와 JWE를 함께 보내거나 401 뒤 다른 방식을 자동 재시도하지 않는다. 900초 TTL은 레거시 소스의 기본값이지 실제 설치의 확인값이 아니다.

## 설정·마이그레이션 계약

현재 설정 버전은 9이며 Dashboard 대상은 `id`, `name`, `base_url`, `secret_ref`, `disabled`를 저장한다. 비밀 원문은 Windows Credential Manager에 별도로 저장된다. 인증 모드는 일반 설정에서 비밀값을 다시 저장하거나 변환하는 수단이 아니다. [설정 모델](../../main.go#L97-L107), [Dashboard 대상 모델](../../dashboard.go#L17-L24)

설정 버전을 올린다면 새 `auth_mode` 필드를 선택 필드로 추가하고 버전 9 이하의 누락값을 `legacy_bearer`로 해석한다. 마이그레이션은 기존 `SecretRef`, 대상 ID, 활성화 상태와 서비스 환경 매핑을 보존하고 비밀값을 읽거나 재저장하지 않는다. 같은 규칙을 현재 설정 읽기, 설정 가져오기, 대상 편집, 연결 테스트 기록, 복구 경로에 적용한다. 설정 초안은 현재 Dashboard 대상의 인증 모드를 왕복하지 않으므로 초안 스냅샷과 저장 변환에도 이 필드를 추가해야 한다. [초안 변환](../../setup_draft_config_store.go#L127-L133), [초안 복원](../../setup_draft_config_store.go#L204-L212)

모드 변경은 사용자의 명시적 선택과 새 입력에만 반응한다. 버전 문자열 탐지, 실패 응답, 401 또는 값 모양으로 저장된 credential을 재분류하지 않는다. 인증 값을 MCP 인자·대화 내용·일반 설정 내보내기에 복사하지 않는다.

## 첫 조회와 namespace 목록 권한

현재 `registered_target_connection_test`는 Namespace 전체 목록을 호출한다. 따라서 Namespace 목록 권한이 없으면 대상 연결 테스트가 실패할 수 있다. 그러나 서비스 환경은 Dashboard 대상과 namespace/deployment를 이미 저장된 매핑으로 가리키며, MCP의 Deployment 상태 조회는 그 매핑만 사용한다. [연결 테스트 구현](../../main.go#L1199-L1214), [저장 매핑 사용](../../dashboard.go#L432-L481)

첫 읽기의 최소 경로는 사용자가 알고 있는 namespace와 Deployment를 로컬 환경 매핑에 지정하고, `dashboard_deployment_status`로 해당 Deployment 상세를 읽는 것이다. `dashboard_namespaces`는 탐색 편의 기능으로 둔다. namespace 목록 요청이 거부되거나 불완전해도 등록, 명시 매핑, 지정 Deployment 읽기를 막지 않는다. 목록 권한이 없으면 “namespace 자동 목록을 읽을 수 없음”을 표시하고 알려진 namespace를 입력하도록 안내한다. Dashboard UI의 `namespaceFallbackList` 설정이 특정 목록을 보여준다는 사실만으로 API의 전체 목록 권한이나 실제 설치 동작을 추정하지 않는다.

레거시 Dashboard의 Deployment 상세 endpoint는 `DeploymentDetail`을 반환하며 Deployment 조회 외에 Pod, ReplicaSet, event, HPA 등 보조 자료를 모으고 `errors`에 비치명 오류를 포함한다. 따라서 first-read 결과에서 Deployment 상태와 보조 진단 오류를 구분해야 한다. 원격 오류 문자열을 그대로 MCP 결과에 전달하지 말고 고정된 오류 코드나 짧은 안전 안내로 투영한다. [공식 endpoint 등록](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/handler/apihandler.go#L260-L278), [공식 DeploymentDetail 구조](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/resource/deployment/detail.go#L33-L94)

## `/api/v1` 응답·경로 비교

아래 표는 레거시 공식 `v1.7.0` 코드의 API handler와 resource 타입을 현재 파서와 비교한 것이다. 경로가 등록되어 있다는 사실은 사용자 대상의 권한·프록시·TLS·버전 호환성을 증명하지 않는다.

| 읽기 | 레거시 upstream 계약 | 현재 코드와 영향 |
| --- | --- | --- |
| Namespace 목록 | `GET /api/v1/namespace`; `listMeta`, `namespaces[].objectMeta.name`, `errors` | 이름과 목록 메타는 파싱한다. `errors`를 무시하므로 보조 오류가 있는 응답을 완전 성공처럼 보일 수 있다. 전체 목록 권한을 첫 Deployment 읽기의 조건으로 삼으면 안 된다. |
| Deployment 상태 | `GET /api/v1/deployment/{namespace}/{deployment}`는 `DeploymentDetail`을 반환한다. `objectMeta`와 `statusInfo.{replicas,updated,available,unavailable}`가 있다. `conditions` 최상위 필드는 없다. `newReplicaSet`, `podList`, `oldReplicaSetList`, `eventList`, `errors`도 응답에 들어간다. | 현재 파서는 최상위 `conditions`를 필수로 요구하므로 공식 형식의 상세 응답을 거부한다. replica 수 필드는 맞는다. Deployment 상태 최소 투영을 지원하고 존재하는 `errors`는 안전한 코드로만 요약해야 한다. |
| Deployment events | `GET /api/v1/deployment/{namespace}/{deployment}/event`; `EventList.events[]`에는 `objectMeta.name`, `type`, `reason`, `count`, `firstSeen`, `lastSeen`, `object`가 있다. | 필수 타입·이름·시간·count는 맞는다. `objectName`은 공식 필드가 아니며 현재 파서는 이를 항상 비워둘 수 있다. 공식 `object`는 세부 참조 문자열이므로 객체 이름으로 오해해선 안 된다. |
| 현재 ReplicaSet 발견 | Deployment 상세 안의 `newReplicaSet` | 공식 handler에는 `/deployment/{namespace}/{deployment}/newreplicaset` route가 없다. 현재 [dashboard.go](../../dashboard.go#L1007-L1021)는 이 경로를 요청하고 응답 최상위 `objectMeta.name`을 기대한다. 따라서 ReplicaSet 기반 Pod 탐색은 레거시 upstream에서 막힌다. `newReplicaSet.objectMeta.name`은 Deployment 상세에서 읽는다. |
| 과거 ReplicaSet | `GET /api/v1/deployment/{namespace}/{deployment}/oldreplicaset`는 `ReplicaSetList`를 반환한다. | 현재 목록 이름·메타 구조는 호환 가능하다. 공식 목록의 `errors`도 확인해 비완전 결과를 조용히 완전 목록으로 처리하지 않아야 한다. |
| ReplicaSet Pod 목록 | `GET /api/v1/replicaset/{namespace}/{replicaSet}/pod`는 `PodList`를 반환한다. Pod의 `podStatus.status`, `podStatus.podPhase`, `podStatus.containerStates`, `restartCount`, `nodeName`을 쓴다. | 현재 파서는 최상위 `status`와 `containerStatuses[]`를 필수로 하므로 공식 `PodList`를 거부한다. `containerStates`는 이름과 `ready`가 없는 Kubernetes 상태 union이므로 이름/ready를 꾸며내면 안 된다. |
| 컨테이너 이름 | `GET /api/v1/pod/{namespace}/{pod}/container`; handler는 `{containers:[...]}` 목록을 반환한다. | Pod 리스트의 상태 정보와 별도로, 요청한 Pod의 등록 여부를 먼저 확인한 뒤 해당 Pod의 컨테이너 이름만 읽어 로그 대상을 검증할 수 있다. `PodDetail`의 환경 변수·명령을 가져올 필요가 없다. |
| Pod logs | `GET /api/v1/log/{namespace}/{pod}/{container}`; `info.{podName,containerName,truncated}`, `selection`, `logs[].{timestamp,content}`. 조회 선택 query는 `referenceTimestamp`, `referenceLineNum`, `offsetFrom`, `offsetTo`, `logFilePosition`이다. | 현재 경로·주요 응답 필드·query 이름은 맞는다. `selection`은 무시해도 된다. 기존 라인/바이트/MCP 응답 제한과 Pod/컨테이너 매핑 검증을 유지한다. |

근거 링크: [공식 `/api/v1` 경로](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/handler/apihandler.go#L100-L105), [namespace·Deployment·ReplicaSet 경로](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/handler/apihandler.go#L214-L278), [namespace 경로](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/handler/apihandler.go#L347-L359), [Pod·logs 경로](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/handler/apihandler.go#L214-L260), [NamespaceList](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/resource/namespace/list.go#L17-L36), [PodList](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/resource/pod/list.go#L28-L61), [EventList](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/resource/common/event.go#L20-L60), [LogDetails](https://github.com/kubernetes-retired/dashboard/blob/v1.7.0/src/app/backend/resource/logs/logs.go#L65-L133)

## 실제 대상 없이 가능한 검증

현재 [dashboard_test.go](../../dashboard_test.go)는 fake HTTP transport와 JSON fixture로 요청 경로, 출력 투영, 상한, redirect/401/403/과대 응답, MCP 범위 제한을 검증한다. 이 구조를 재사용해 다음 차이를 검증할 수 있다.

- 레거시 대상에 `auth_mode`가 없을 때 읽기·저장·재시작 후에도 `legacy_bearer` 의미와 기존 Bearer 전송을 유지하고, Credential Manager 값은 읽거나 바꾸지 않는다.
- 새 `kubernetes_bearer` 대상은 로그인 화면 입력 토큰을 Bearer로 보내며, UI/오류/설정 내보내기/MCP에 비밀이 나타나지 않는다. 기존 대상에서 모드만 바꾸고 비밀 입력을 비운 경우 변경을 거부하거나 이전 모드를 보존한다.
- 401 이후 JWE나 다른 header를 보내는 두 번째 요청이 없고, 한 요청에 `Authorization`과 `jweToken`을 함께 보내지 않는다.
- 공식 v1.7.0 `NamespaceList`, `DeploymentDetail`, `EventList`, `ReplicaSetList`, `PodList`, `LogDetails` 형태를 그대로 담은 fixture로 각 parser의 성공·부분 오류·상한을 검증한다. 현재 synthetic fixture는 upstream 계약 fixture와 구분한다.
- 요청한 namespace/deployment 매핑을 가진 상태에서 Namespace 목록 권한 실패를 주입해도 첫 Deployment 상태 읽기를 별도로 시도할 수 있다. 이 테스트는 목록 실패를 연결 전체 실패나 “대상 없음”으로 분류하지 않는지 확인한다.
- `newreplicaset` 호출이 제거되고 `newReplicaSet`이 Deployment 상세에서 파싱되는지, Pod 상태가 `podStatus`에서 투영되는지, 로그 전에 컨테이너 이름만 조회하는지 확인한다.
- config 9→10, 기존 설정 가져오기/초안 저장 후 복원에서 인증 모드가 보존되고 모드 변경 시 기존 연결 테스트 결과가 무효화되는지 검증한다.

기존 redirect 차단, 다른 origin 차단, 요청 제한, 응답 상한, 민감 데이터 필터링, 등록된 service bundle/environment 범위 테스트는 그대로 유지한다. 이 작업에서는 테스트를 실행하지 않았고 사용자의 서비스에 네트워크 요청을 보내거나 비밀을 조회하지 않았다.

## 완료 판정

구현 리뷰가 시작되기 전, 대상이 레거시 단일 서버 `v1.7.0`인지 2025년 `web/v1.7.0`을 포함한 별도 API 컴포넌트 구성인지 확인한다. 그 다음 인증 모드가 새 입력·구버전 설정·초안·가져오기에서 명확히 구분되고, 공식 응답 fixture로 첫 Deployment 읽기가 통과하며, Namespace 목록 권한을 첫 조회 조건으로 요구하지 않는지 확인한다. 이 조건이 충족되어도 실제 사용자 설치 성공은 별도 검증으로 남는다.
