# v0.1.0-rc.7

회사별 주소와 흔한 Jenkins 작업명을 처리하는 범위를 보완한 Windows x64 사전 공개 후보입니다.

- 일반 `github.com`과 `api.github.com`은 공식 REST API 경로를 사용합니다. 사내 GitHub Enterprise Server는 등록한 호스트·포트와 `/api/v3`를 유지합니다. 저장소 읽기, PR 읽기, 초기 연결 시험에 같은 주소 처리 코드를 사용합니다.
- Jenkins는 등록한 기본 주소의 하위 경로를 유지하고, 공백·한글·괄호·`+`가 있는 작업명을 URL에 한 번만 인코딩합니다. URL 후보와 연결 시험도 같은 규칙을 따릅니다.
- 서비스 요청 대기 시간을 기존 10~12초에서 최대 30초로 늘렸습니다. 호출자 취소는 계속 적용하며 자동 재전송은 추가하지 않았습니다.
- VPN·DNS·프록시·인증서·SSO 확인과 실패한 대상만 고쳐 다시 시험하는 절차를 한국어 사용 안내와 영어 AI 안내에 추가했습니다. TLS 검사, 대상 범위, redirect 차단과 Jenkins의 실행 결과 불명확 시 재시도 금지는 유지합니다.

설치는 `START_HERE.ko.md`, 클라이언트 등록은 ZIP 안의 `CLIENT_SETUP.md`, 에이전트 위임은 별도 `AGENT_SETUP.md`를 사용하세요. 회사 환경의 구체적인 주소 입력과 연결 조건은 `USER_GUIDE.ko.md`를 참고하세요.

검사 결과와 남은 범위는 `VALIDATION_SUMMARY.ko.md`에 기록합니다. 모의 검사는 실제 회사 서비스, 프록시·SSO, Windows 자격 증명 저장소 또는 실제 CLI·Desktop 연동의 성공을 보장하지 않습니다.
