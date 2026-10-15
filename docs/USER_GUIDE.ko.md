# 이럴 때 이렇게 쓰세요

**이 안내는 RC4 ZIP에 오프라인 문서로 포함되며, RC4 앱 안에서도 `/guide` 화면으로 열 수 있습니다. RC3 설치 파일에는 이 화면이 없습니다.** 아래 요청문은 연결된 AI 클라이언트에 붙여 넣는 자연어 예시입니다. JSON 블록은 MCP 도구에 전달되는 인자 모양을 보여 줍니다. 이 문서는 어느 것도 실행하지 않으며, 실제 연결·응답·권한을 확인한 결과가 아닙니다. `Example` 이름, 번호, `.invalid` 주소는 모두 가상 값이므로 실제 사용 때 등록된 이름과 응답으로 바꾸세요.

Local Agent Harness는 등록된 서비스 범위 안에서 MCP 도구를 제공합니다. 아직 연결 대상이 없다면 **연결 대상** 화면에서 주소를 확인하고 초안을 저장한 뒤, 임시 확인과 작업 범위 검토를 거쳐 직접 등록하세요. 주소 확인·초안 저장·대상 등록·서비스 연결 테스트·AI 클라이언트의 실제 서비스 조회는 각각 다른 동작입니다.

## 처음 한 번만 연결하기

### 1. 주소를 넣고 후보를 확인합니다

**연결 대상** 화면 맨 위의 “1 · 서비스 주소로 연결 대상 추가”에서 **서비스**를 선택하고 **HTTPS 서비스·저장소·작업 주소**를 입력한 다음 **주소 확인**을 누릅니다. 주소에는 사용자 이름, 비밀번호, 토큰, 쿼리 문자열(`?` 뒤), 프래그먼트(`#` 뒤)를 넣지 마세요. 주소 확인은 브라우저에서 로컬 앱으로 후보를 보내며 원격 서비스에 접속하거나 설정을 저장하지 않습니다.

아래 주소는 가상 예시입니다. 어떤 값을 후보로 보여 주는지 확인한 뒤 실제 조직의 주소와 이름으로 바꾸세요.

| 서비스 선택 | 가상으로 입력할 URL | 후보로 채워지는 값 | 사용자가 확인하거나 추가할 값 |
| --- | --- | --- | --- |
| GitHub Enterprise | `https://github.example.invalid/Acme/Widget.git` | 서비스 주소 `https://github.example.invalid`; 저장소 `Acme/Widget` | 대상 이름과 이 Enterprise 서버용 토큰 |
| Jenkins | `https://ci.example.invalid/jenkins/job/Platform/job/Release-Gate/` | 기본 주소 `https://ci.example.invalid/jenkins`; 고정 작업 경로 `Platform/Release-Gate` | 대상 이름, Jenkins 사용자 이름·API 토큰, 경로가 실제 context path와 작업 경로에 맞는지 확인. 환경 이름표는 필요할 때 입력 |
| Kubernetes Dashboard | `https://dashboard.example.invalid/proxy/web/` | 기본 주소 `https://dashboard.example.invalid/proxy/web` | 대상 이름과 설치된 Dashboard 또는 proxy에 필요한 API Bearer. 이 주소는 버전이나 인증 방식을 알아내지 않음 |

후보 카드의 “주소 후보 확인”에서 항목을 읽고 **새 대상 양식에 값 적용**을 누릅니다. 주소에서 찾지 못한 필수 값이 있으면 양식에서 채우고, 추정한 Jenkins 경로와 Dashboard proxy 경로를 다시 확인하세요. URL 후보는 사이트·저장소·작업이 실제로 존재하는지 알려 주지 않습니다.

### 2. 자격 증명을 준비하고 진행 상황을 저장합니다

대상 양식의 **인증 준비 안내**를 열고 자격 증명 필드 옆 설명을 확인합니다. GitHub Enterprise 토큰은 사용 중인 서버 버전의 공식 문서에서 발급 위치와 권한을 확인하세요. [GitHub Enterprise Server 3.20의 personal access token 안내](https://docs.github.com/en/enterprise-server%403.20/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)는 개인 액세스 토큰 발급과 관리 방법을 설명하며, [fine-grained token 권한 표](https://docs.github.com/en/enterprise-server%403.20/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens)는 REST endpoint별 필요한 권한을 찾는 데 도움을 줍니다. 서버 버전이 다르면 그 버전에 맞는 문서를 사용하고 관리자에게 조직 승인 여부를 확인하세요.

Jenkins API token은 해당 Jenkins 계정으로 로그인한 뒤 개인 설정의 **Security**에서 만들고 관리할 수 있습니다. [Jenkins scripted-client 인증 안내](https://www.jenkins.io/doc/book/system-administration/authenticating-scripted-clients/)와 [접근 제어 문서](https://www.jenkins.io/doc/book/security/access-control/)를 확인하고, 계정에 지정 job의 읽기 권한만 필요한 범위로 부여됐는지 관리자와 확인하세요.

Dashboard의 토큰 발급 방법은 설치 방식과 proxy 설정에 따라 달라질 수 있습니다. 관리자나 해당 설치 문서에서 API용 Bearer 값과 namespace 또는 지정 Deployment의 읽기 권한을 확인하세요. UI의 인증 도움말에 적힌 대로 앱은 입력값을 그대로 Bearer 인증에 사용하며 로그인 교환이나 버전 감지를 하지 않습니다. 화면의 `1.7.0` 숫자만으로 backend나 인증 형식을 판단하지 마세요.

초안을 이어서 작성하고 싶다면 양식에서 **안내에 따라 진행 상황 저장**을 누릅니다. 저장된 진행 상황은 연결 대상 화면의 “연결 설정 이어하기”에서 **저장된 진행 상황 새로고침**을 누르면 목록에 나타납니다. 해당 서비스와 시간을 확인하고 **열기**를 누르세요. 편집 중이던 값이 있을 때는 **저장하지 않은 양식 값을 버리고 이어하기** 또는 **현재 양식 유지** 중 하나를 고릅니다. 초안에는 정규화된 서비스 주소 등 일반 필드가 남지만, 붙여 넣은 원문 URL, 비밀번호, 토큰, 저장된 자격 증명 참조는 남지 않습니다. 다시 열었을 때는 자격 증명을 다시 입력하거나 이름 있는 자격 증명을 다시 고르세요. 초안만 저장한 상태에서는 MCP 대상이 만들어지지 않습니다.

### 3. “임시 연결 확인 실행”을 누릅니다

양식의 자격 증명 입력란에 준비한 값을 넣고 **임시 연결 확인 실행**을 누릅니다. 이 버튼을 누르면 저장 예정 주소와 선택한 자격 증명으로 제한된 원격 확인 요청을 보냅니다. 성공하면 화면에 확인 성공이 표시되고 초안에 안전한 확인 결과가 남습니다. 대상 설정, 비밀 저장소, 일반 연결 테스트 이력은 바뀌지 않습니다. 이 임시 확인은 5분 뒤 만료되며, 주소·필드·자격 증명을 바꾸거나 초안을 다시 열면 다시 확인하세요.

### 4. 범위를 읽고 “이 대상 등록”을 누릅니다

확인 성공 후 **고정 작업 범위 검토**를 누릅니다. 포함되는 읽기 기능과 추가 설정이 필요한 항목, 실행 기능의 주의사항을 읽은 다음 확인 상자 **고정 범위를 확인했으며 이 대상을 등록하겠습니다**를 선택하세요. 개별 MCP 도구의 허용 여부를 이 화면에서 하나씩 고르는 기능은 없습니다. 준비가 됐으면 **이 대상 등록**을 누릅니다.

이 단계는 새 대상 하나를 등록합니다. 주소가 기존 대상과 같아도 기존 대상을 자동 수정하지 않습니다. 기존 대상을 바꾸려면 기존 대상 양식에서 직접 편집하세요. 등록이 끝나면 화면에 대상이 저장됐다고 표시됩니다. 이 단계에서는 별도 연결 테스트나 AI 클라이언트의 읽기를 실행하지 않습니다. **대상을 열고 별도로 연결 테스트 실행**으로 서비스 연결을 확인하거나, **AI 앱 설정 안내 열기**에서 클라이언트 설정으로 이동하세요. 직접 입력한 토큰은 등록을 확정할 때 Windows Credential Manager에 저장하도록 구현되어 있고, 이름 있는 자격 증명을 선택하면 기존 값을 사용합니다. 저장소의 합성 테스트는 실제 사용자 Windows Credential Manager의 저장·삭제를 시험하지 않았습니다.

## 무엇을 확인했는지 구분하기

| 동작 | 대상 서비스에 요청하나요? | 설정/이력에 어떤 영향이 있나요? | 이것만으로 첫 외부 읽기인가요? |
| --- | --- | --- | --- |
| URL 후보 미리보기 | 아니요 | 없음 | 아니요 |
| 일반 초안 저장 | 아니요 | 일반 설정 진행 상황을 보관 | 아니요 |
| 임시 연결 확인 | 예, 버튼을 눌렀을 때 제한된 요청 | 초안에 확인 결과를 보관; 대상 설정과 일반 연결 이력은 변경하지 않음 | 아니요 |
| MCP 카탈로그 조회 (`registered_targets`) | 아니요 | 설정에 등록된 이름과 사용할 수 있는 도구를 읽음 | 아니요 |
| 등록 대상 연결 테스트 | 예, 등록된 서비스로 제한된 읽기 요청 | 성공/실패 이력이 로컬 설정에 추가될 수 있음 | 아니요 |
| 허용된 MCP 도구로 성공한 외부 서비스 읽기 | 예 | 준비 상태 점검에 실제 첫 조회 기록을 표시 | 예 |

등록 목록을 보거나 별도 연결 테스트에 성공했다는 사실은 AI 클라이언트가 외부 서비스 데이터를 읽었다는 뜻이 아닙니다. MCP 연결 설정과 도구 목록을 확인한 다음, 원하는 서비스를 읽도록 AI 클라이언트에 직접 요청하세요.

등록 대상 목록은 외부 요청 없이 `{}` 인자로 읽습니다.

```json
{}
```

도구 이름은 `registered_targets`입니다. 서비스 묶음 목록도 `registered_service_bundles`에 같은 빈 인자 `{}`를 전달합니다.

대상 등록 뒤 AI 클라이언트 설정 전에 서비스 endpoint를 한 번 확인하려면 `registered_target_connection_test`를 사용할 수 있습니다. 예를 들어 등록된 GitHub 대상 “Example GH”는 다음처럼 지정합니다.

```json
{
  "adapter": "github",
  "target": "Example GH"
}
```

`adapter`에는 `github`, `jenkins`, `harbor`, `dashboard` 중 하나를 넣고 `target`에는 등록된 이름을 그대로 씁니다. 이 작업은 등록 주소로 제한된 읽기 요청을 보내고 로컬 연결 이력을 갱신할 수 있습니다. 성공해도 AI 클라이언트의 첫 조회로 기록되지는 않습니다.

아래 회색 요청 예시는 연결된 AI 클라이언트에 붙여 넣으세요. JSON은 각 도구의 인자 구조 예시입니다. 두 형식 모두 이 안내가 자동 실행하지 않습니다.

## 회사 환경의 주소와 연결 조건

예제 주소에 고정되어 있지 않습니다. 대상마다 사용자가 확인한 실제 주소·포트·리소스 범위를 저장합니다.

| 서비스 | 등록할 주소와 경로 | 처리 방식 |
|---|---|---|
| 일반 GitHub | 주소 `https://github.com`, 저장소 `owner/repo` | 공식 REST 호스트 `https://api.github.com/repos/owner/repo`로 요청합니다. `https://api.github.com`을 등록해도 같은 API를 사용합니다. |
| 사내 GitHub Enterprise Server | 주소 `https://git.company.invalid` 또는 `https://git.company.invalid:8443`, 저장소 `owner/repo` | 등록한 호스트·포트를 유지하고 `/api/v3/repos/owner/repo`를 붙입니다. 주소 필드에는 `/api/v3`나 저장소 경로를 넣지 않습니다. |
| Jenkins | 기본 주소 `https://ci.company.invalid:8443/proxy/jenkins`, 작업 경로 `개발팀/Release Gate` | 기본 주소를 유지하고 `/job/개발팀/job/Release Gate`를 URL 인코딩해 요청합니다. 작업 경로에는 `/job/` 표시 대신 작업·폴더 이름만 `/`로 구분해 넣습니다. |

Jenkins 작업명의 공백·한글·괄호·`+`를 지원합니다. Jenkins 주소 후보의 작업명 부분에는 브라우저에서 복사한 인코딩 URL이나 공백·한글을 그대로 넣을 수 있습니다. 기본 주소의 context path는 공백·한글을 지원하지 않으므로, 작업명 부분과 구분해 확인하세요. 후보에서 추정한 기본 주소와 작업 경로는 승인 전에 확인하세요. `..`, 이중 인코딩, 경로 구분자를 숨긴 값, 제어 문자는 허용하지 않습니다.

서비스 요청은 최대 30초를 기다리며, 클라이언트가 먼저 취소하면 더 일찍 종료됩니다. 실패했다고 자동으로 빌드를 다시 실행하지 않습니다. 특히 Jenkins 실행 결과가 불명확하면 기존 queue/build를 확인한 뒤 재실행 여부를 결정하세요.

회사 VPN과 DNS가 먼저 동작해야 합니다. Windows에서 실행 중인 앱 프로세스가 받은 `HTTPS_PROXY`·`HTTP_PROXY`·`NO_PROXY` 환경변수는 Go 기본 HTTP 전송에서 사용하지만, Windows 브라우저의 PAC·자동 프록시·통합 인증을 그대로 가져오지는 않습니다. 환경변수를 바꾼 뒤에는 앱과 stdio MCP 서버를 다시 시작하세요. 사내 인증서는 Windows가 신뢰하는 인증서 체인으로 준비하고, 연결 실패를 해결하려고 인증서 검증을 끄지 마세요. 앱이 회사의 VPN·프록시·인증서를 자동 설정하지는 않습니다.

브라우저 로그인 성공만으로 API 인증이 완료되지는 않습니다. GitHub PAT의 조직 SSO 승인, Jenkins 사용자 이름·API 토큰 권한, proxy의 API 접근 정책을 확인하세요. 로그인 페이지로의 redirect, HTTP 전용 서비스, GitHub Enterprise의 별도 API prefix, 토큰 교환이 필요한 SSO는 현재 자동 처리하지 않습니다. 해당 설치의 API 주소·인증 방식을 관리자와 확인한 뒤 설정을 고칩니다. 실패한 항목만 다시 시험하고, 다른 대상의 저장된 설정을 지우거나 전체 설치를 반복할 필요는 없습니다.

설정 후보, 연결 시험, 실제 MCP 읽기를 구분하세요. 등록 뒤 대상별로 연결 시험을 하고, 이어서 허용한 읽기를 한 번 수행해 `setup_status`의 첫 읽기 기록을 확인합니다. 모의 환경 통과는 회사 서비스의 성공 보장이 아닙니다.

## GitHub: 저장소 기본 정보나 PR 메타데이터를 읽습니다

GitHub 도구는 정확히 등록된 대상의 저장소와 PR(풀 리퀘스트) 메타데이터를 읽습니다. `github_repository`는 이름, 설명, 기본 브랜치, 공개 여부와 같은 저장소 정보를 제공합니다. 이 도구는 저장소 파일이나 diff를 읽지 않으므로 “전체 코드 리뷰”나 코드 취약점 검토를 했다고 말할 수 없습니다.

가상 등록 대상 이름 `Example GH`의 기본 정보를 요청하는 자연어 예시:

> 등록된 GitHub 대상 “Example GH”에서 저장소 기본 정보를 읽어 주세요. 설명, 기본 브랜치, 공개 여부만 알려 주고 코드를 읽거나 전체 코드 리뷰를 했다고 말하지 마세요. 어떤 것도 변경하지 마세요.

정확한 MCP 도구 호출 인자:

```json
{
  "target": "Example GH"
}
```

도구 이름은 `github_repository`입니다. “Example GH”는 이 안내의 가상 표시 이름입니다. 실제 호출에서는 활성 등록 목록에 보이는 대상 이름을 정확히 넣으세요.

특정 Pull Request의 메타데이터를 읽으려면 양의 PR 번호를 지정합니다. 다음 번호도 가상 예시입니다.

```json
{
  "target": "Example GH",
  "number": 42
}
```

이 호출의 도구 이름은 `github_pull_request`입니다. 권한이 없거나 번호가 잘못되면 실제 서비스 결과가 돌아오지 않습니다. 소스 파일과 diff를 읽는 도구가 아니므로 전체 코드 리뷰를 요청하지 마세요.

## Jenkins: 등록된 작업의 최근 상태를 봅니다

읽기 전용의 작업 상태 확인에는 `jenkins_registered_job`을 사용합니다. 아래 요청은 상태만 알려 달라고 하고 실행·변경은 하지 말도록 범위를 분명히 합니다.

> 등록된 Jenkins 대상 “Example CI”에 연결된 고정 작업의 최근 상태와 마지막 빌드 결과만 읽어 주세요. 빌드를 시작하거나 설정을 바꾸지 마세요.

```json
{
  "target": "Example CI"
}
```

“Example CI”는 가상 이름입니다. 이 도구는 정확히 등록된 대상에 묶인 작업을 읽습니다. 대기열 항목이나 로그를 확인하려면 그 대상에서 확인한 대기열 ID 또는 빌드 번호가 필요합니다. 대기열 도구 인자는 `jenkins_registered_queue_item`의 `{"target":"Example CI","queue_id":123}` 모양입니다. 로그 도구는 시작 위치를 지정해 제한된 구간만 읽습니다.

```json
{
  "target": "Example CI",
  "build_number": 17,
  "start_offset": 0
}
```

위 인자는 `jenkins_registered_build_log`의 구조를 보여주는 예시이며, 빌드 `17`과 시작 위치 `0`은 허구입니다. 실제 출력의 다음 시작 위치를 사용해 후속 구간을 요청하세요. `jenkins_run_registered_job`은 읽기가 아니라 등록된 작업을 실제로 실행합니다. 해당 작업을 비운영 대상으로 사전 승인했다면 호출별 로컬 확인 없이 실행될 수 있습니다. 그 외 활성 작업은 일회성 로컬 브라우저 확인이 필요합니다. 실행 전 대상과 영향을 확인하세요.

## Kubernetes Dashboard: 먼저 보이는 네임스페이스를 읽습니다

초기 확인에는 `dashboard_namespaces`를 사용합니다. 이 도구는 정확히 등록된 Dashboard 대상이 볼 수 있는 네임스페이스 이름을 최대 100개까지 읽습니다. URL이나 네임스페이스 경로를 인자로 바꾸어 임의 대상을 지정할 수 없습니다.

> 등록된 Dashboard 대상 “Example Dashboard”에서 볼 수 있는 Namespace 이름만 읽어 주세요. 리소스를 만들거나 수정하거나 삭제하지 마세요.

```json
{
  "target": "Example Dashboard"
}
```

이 읽기는 배포 상태 진단용 매핑을 자동으로 만들지 않습니다. 배포·이벤트·Pod·로그 도구는 `registered_service_bundles`에 저장된 서비스 묶음과 환경의 사전 매핑을 사용합니다. 해당 환경에 정확한 Dashboard 대상, 네임스페이스, 배포가 등록된 뒤에만 다음과 같이 진단할 수 있습니다.

> “Example Checkout” 서비스 묶음의 “staging” 환경에서 매핑된 배포의 상태, 관련 이벤트, Pod 상태를 읽어 주세요. 로그를 가져오거나 리소스를 변경하지 마세요.

```json
{
  "service_bundle": "Example Checkout",
  "environment": "staging"
}
```

도구 이름은 `dashboard_deployment_diagnosis`입니다. 이 진단은 상태, 이벤트, Pod 목록을 함께 읽으며 한 시점의 원자적 스냅샷은 아닙니다. 개별 로그는 `dashboard_deployment_pod_logs`로 Pod와 컨테이너를 지정합니다. Dashboard 화면의 `1.7.0` 표시는 설치된 서버나 인증 방식의 근거가 아닙니다. 앱은 입력한 Bearer 값을 그대로 사용하며 버전을 감지하거나 로그인 토큰을 교환하지 않습니다. 실제 설치의 API 주소·proxy·토큰 종류·읽기 권한은 관리자에게 확인하고 추측하거나 자동으로 다른 방식으로 재시도하지 마세요.

로그 요청은 진단이나 Pod 목록에서 확인한 현재 매핑 배포의 Pod와 컨테이너 이름만 받습니다. 다른 Pod/컨테이너 이름을 직접 지정해 임의 리소스를 읽을 수 없습니다. 네임스페이스 목록 확인이 실패하더라도, 그것만으로 사전에 매핑된 특정 배포 읽기까지 불가능하다고 단정하지 마세요. 필요한 권한 범위는 관리자에게 확인하세요.

```json
{
  "service_bundle": "Example Checkout",
  "environment": "staging",
  "pod": "checkout-7dd4b",
  "container": "web"
}
```

이 `dashboard_deployment_pod_logs` 인자도 가상 예시입니다. 실제 Pod와 컨테이너는 해당 매핑에서 확인한 값을 사용하세요.

## Harbor, 서비스 묶음, SSH와 Python은 언제 쓰나요?

| 상황 | 도구와 인자 예시 | 구분할 점 |
| --- | --- | --- |
| Harbor에서 등록된 저장소 artifact나 quota를 읽을 때 | `harbor_repository_artifacts` 또는 `harbor_project_quota` — `{"target":"Example Harbor"}` | Harbor는 등록 대상 MCP 읽기를 지원합니다. 주소 기반 안내 등록은 GitHub Enterprise, Jenkins, Dashboard를 대상으로 합니다. Harbor는 기존 연결 양식에서 관리합니다. |
| 저장한 저장소와 환경별 연결을 살펴볼 때 | `registered_service_bundles` — `{}` | 서비스 묶음, 환경, 연결 이름을 보여 주는 로컬 목록입니다. 서비스에는 접속하지 않고 Dashboard 매핑도 만들지 않습니다. |
| 저장한 SSH 대상에서 허용된 작업을 실행할 때 | 먼저 `registered_ssh_targets` — `{}`; 실행 도구는 `run_ssh_operation` | 목록에 표시된 대상 ID와 작업 ID를 사용합니다. 입력으로 임의 서버 주소·명령·자격 증명을 받지 않습니다. 변경·삭제 작업은 저장된 승인 정책을 따릅니다. 취소나 연결 끊김 뒤 원격 결과가 불분명하면 자동으로 반복하지 마세요. |
| 저장한 로컬 Python 작업을 실행할 때 | 먼저 `registered_python_tasks` — `{}`; 실행 예 `python_run_registered_task` — `{"task":"Example report"}` | 목록은 파일이나 네트워크 접근을 확인하지 않습니다. 실행 도구는 등록된 로컬 작업을 실제로 수행하므로 목적을 확인한 뒤 선택하세요. SSH/Python 실행만으로 서비스 첫 조회가 기록되지는 않습니다. |

SSH 작업의 인자 구조는 다음과 같습니다. 자리표시자를 `registered_ssh_targets`가 돌려준 실제 ID와 필수 항목으로 바꾸세요. 이것은 그대로 실행하는 값이 아닙니다.

```json
{
  "target_id": "<catalog에서 확인한 target ID>",
  "operation_id": "<catalog에서 확인한 operation ID>",
  "parameters": {}
}
```

## AI 클라이언트에서 첫 읽기를 확인합니다

MCP 연결은 현재 제공하는 `--mcp` stdio 서버를 해당 클라이언트에 등록해야 합니다. CLI별 등록 명령, 사용자 범위와 연결 확인은 [MCP 클라이언트 수동 설정 안내](CLIENT_SETUP.md)를 따르세요. 등록된 도구를 사용할 때는 서비스를 읽어도 되는지 먼저 확인하고 구체적인 범위를 자연어 요청에 적습니다.

- **Codex CLI**는 현재 클라이언트 문서의 stdio 등록 예시를 따릅니다.
- **ChatGPT 데스크톱 앱의 Codex 기능**은 공식 문서에서 MCP의 STDIO 서버 연결과 Codex CLI·IDE와의 설정 공유를 안내합니다. 여기서 말하는 것은 Codex 기능이며 일반 ChatGPT 대화나 Work 대화에 같은 연결이 된다는 뜻은 아닙니다. 이 저장소에서 Local Agent Harness를 실제 앱에 설치하거나 호출해 보지는 않았습니다. 설정을 진행할 때는 [OpenAI 공식 Codex MCP 안내](https://developers.openai.com/codex/mcp)를 따라가세요.
- **Claude Code CLI**는 stdio 서버를 사용자 범위로 등록할 수 있습니다. [Claude Code 공식 MCP 안내](https://code.claude.com/docs/en/mcp) 및 이 저장소의 클라이언트 설정 문서를 참고하세요.
- **Claude Desktop**은 CLI와 다른 경로로 `.mcpb` 확장 패키지를 설치합니다. 메뉴와 절차는 [Claude Desktop 패키지 안내](CLAUDE_DESKTOP.md) 및 [Anthropic의 로컬 MCP 안내](https://support.claude.com/en/articles/10949351-getting-started-with-local-mcp-servers-on-claude-desktop)를 참고하세요. 이 저장소에서 제공한 패키지는 서명되지 않은 미리보기이며, 실제 Claude Desktop 설치와 호출은 확인하지 않았습니다.
- **Gemini CLI** 설정은 현재 [Gemini 공식 MCP 문서](https://geminicli.com/docs/tools/mcp-server/)에 따릅니다. 해당 문서는 현재 작업 폴더가 신뢰되지 않으면 stdio 서버가 `Disconnected`로 보일 수 있다고 안내합니다.

클라이언트가 서버와 도구를 보여 주면, 위 GitHub·Jenkins·Dashboard 예시처럼 필요한 데이터만 읽어 달라고 요청하고 응답을 검토하세요. `registered_targets`와 `registered_service_bundles`는 저장된 이름을 확인하는 로컬 목록입니다. `registered_target_connection_test`는 등록한 서비스로 제한된 읽기 요청을 보내고 일반 연결 이력을 갱신할 수 있는 별도 기능입니다. 이 세 가지 결과는 AI 클라이언트가 MCP 도구로 외부 데이터를 성공적으로 읽었다는 증거가 아닙니다.

허용된 MCP 도구가 외부 서비스에서 데이터를 성공적으로 가져온 다음 앱 상단의 **준비 상태 점검**을 열어 그 서비스에 첫 읽기가 표시되는지 확인하세요. 목록 조회, 도구 표시, 연결 테스트, 요청문 복사만으로는 첫 읽기가 기록되지 않습니다. 이 화면은 성공한 MCP 서비스 읽기와 시간을 보여 주지만 어떤 AI 클라이언트가 요청했는지 또는 사람이 결과를 이해했는지는 확인하지 않습니다. Dashboard 진단에서는 진단에 포함된 허용된 읽기 중 하나라도 성공하면 첫 읽기가 표시될 수 있습니다.

## 연결이 실패하거나 초안을 다시 열었을 때

| 표시 또는 상황 | 먼저 확인할 것 |
| --- | --- |
| 임시 확인에서 인증이 거부됨 | 해당 서비스 계정과 토큰인지, 만료·회수되지 않았는지, 읽기 권한이 있는지 확인하세요. 초안에는 자격 증명이 저장되지 않으므로 다시 입력하거나 선택한 뒤 확인을 다시 실행하세요. |
| 읽기 권한이 거부됨 | 지정 저장소·작업 또는 매핑한 Dashboard 리소스에 필요한 권한이 있는지 관리자와 확인하세요. namespace 목록 확인이 실패했다고 해서 사전 매핑된 Deployment 읽기 가능 여부까지 단정할 수는 없습니다. |
| 시간 초과 또는 주소 오류 | HTTPS 주소와 Jenkins 경로 또는 Dashboard proxy 경로를 확인하고 VPN, proxy, 인증서, 방화벽, 서비스 상태를 관리자와 점검하세요. 주소 미리보기는 네트워크 경로를 점검하지 않습니다. |
| Dashboard 인증이 거부됨 | 화면의 버전 숫자로 API 인증을 추측하지 말고, 설치 방식과 proxy에서 요구하는 API Bearer를 관리자와 확인하세요. 앱은 입력값을 그대로 사용하므로 로그인 응답을 임의로 교환하거나 바꿔서 다시 시도하지 마세요. |
| 초안을 다시 열었거나 임시 확인이 만료됨 | 초안에는 자격 증명이 없으며 5분짜리 임시 확인도 다시 사용할 수 없습니다. 자격 증명을 넣고 **임시 연결 확인 실행 → 고정 작업 범위 검토 → 이 대상 등록** 순서로 진행하세요. |
| 등록 결과가 모호하거나 복구 안내가 표시됨 | 버튼을 반복해서 누르거나 설정 파일을 직접 바꾸지 마세요. 화면이 안내하는 수동 복구 절차를 따라 현재 설정과 Windows Credential Manager 항목을 확인하세요. |

모든 URL·표시 이름·번호·응답은 여기서 가상 예시입니다. 실제 사용에서는 허용된 환경의 정확한 등록 대상을 선택하고, 비밀값·실제 사내 주소·개인 경로를 AI 대화, 문서, 로그에 붙여넣지 마세요. RC4 코드와 합성 테스트, 패키지 검증은 실제 공급자 권한, OS Credential Manager, AI client 설치 또는 실제 첫 외부 읽기를 대신하지 않습니다.
