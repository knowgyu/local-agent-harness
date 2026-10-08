package main

import (
	"net/http"
	"strings"
)

type uiLocale string

const (
	uiLocaleEnglish uiLocale = "en"
	uiLocaleKorean  uiLocale = "ko"
)

func uiLocaleForRequest(r *http.Request) uiLocale {
	cookie, err := r.Cookie("lah_lang")
	if err == nil {
		switch cookie.Value {
		case string(uiLocaleEnglish):
			return uiLocaleEnglish
		case string(uiLocaleKorean):
			return uiLocaleKorean
		}
	}
	return uiLocaleKorean
}

// Only fixed, server-authored UI text is translated. Unknown text stays unchanged.
// In particular, target names, response bodies, and MCP errors are not translated.
func localizeUIMessage(locale uiLocale, message string) string {
	if locale != uiLocaleKorean || message == "" {
		return message
	}
	if translated, ok := koreanUIMessages[message]; ok {
		return translated
	}
	for _, suffix := range connectionHistorySuffixes {
		prefix, ok := strings.CutSuffix(message, suffix.english)
		if !ok {
			continue
		}
		if translated, ok := koreanConnectionFailures[prefix]; ok {
			return translated + suffix.korean
		}
	}
	return message
}

var connectionHistorySuffixes = []struct {
	english string
	korean  string
}{
	{" Historical status saved.", " 이력에 결과를 저장했습니다."},
	{" Historical status could not be saved. Retest the current target.",
		" 이력에 결과를 저장하지 못했습니다. 현재 대상을 다시 테스트하세요."},
}

var koreanConnectionFailures = map[string]string{
	connectionFailureMessage(connectionFailureAddress):        "연결 테스트 실패: 서비스에 연결할 수 없습니다. 저장된 HTTPS 주소, 네트워크 또는 VPN 접근, 프록시 경로와 TLS 인증서를 확인하세요.",
	connectionFailureMessage(connectionFailureTimeout):        "연결 테스트 실패: 요청 시간이 초과되었습니다. 네트워크 또는 VPN 연결을 확인하고 다시 시도하세요.",
	connectionFailureMessage(connectionFailureAuthentication): "연결 테스트 실패: 서비스가 저장된 자격 증명을 거부했습니다(401). 올바른 자격 증명으로 바꾼 뒤 다시 테스트하세요.",
	connectionFailureMessage(connectionFailureAccess):         "연결 테스트 실패: 접근이 거부되었습니다(403). 등록한 리소스에 대한 계정의 읽기 권한을 확인하세요.",
	connectionFailureMessage(connectionFailureEndpoint):       "연결 테스트 실패: 서비스가 요청을 다른 곳으로 돌렸거나 엔드포인트 또는 리소스를 찾지 못했습니다. 저장된 기본 URL, API 또는 컨텍스트 경로, 리소스 이름을 확인하세요. 이 결과만으로 리소스 부재와 접근 제한을 구별할 수 없습니다.",
	connectionFailureMessage(connectionFailureRateLimit):      "연결 테스트 실패: 서비스가 요청을 제한하고 있습니다. 잠시 기다린 뒤 다시 시도하세요.",
	connectionFailureMessage(connectionFailureOther):          "연결 테스트 실패: 서비스에서 예상하지 못한 응답을 받았습니다. 저장된 설정을 확인하고 다시 시도하세요.",
}

var koreanUIMessages = func() map[string]string {
	messages := make(map[string]string, 92)
	add := func(english, korean string) { messages[english] = korean }
	add(
		"If the server is disconnected, check its status in /mcp and run claude mcp get local-agent-harness. If it still fails, start a new Claude Code CLI session. A saved-list entry alone does not confirm an active connection.",
		"서버 연결이 끊겼으면 /mcp에서 상태를 확인하고 claude mcp get local-agent-harness를 실행하세요. 그래도 연결되지 않으면 Claude Code CLI 세션을 새로 시작하세요. 저장 목록에 항목이 있어도 현재 연결을 확인한 것은 아닙니다.",
	)
	add("Could not render settings.", "설정 화면을 표시할 수 없습니다.")
	add("Windows login startup is unavailable on this platform.", "이 플랫폼에서는 Windows 로그인 시작을 사용할 수 없습니다.")
	add("Resident MCP will start when you sign in to this Windows account.", "이 Windows 계정에 로그인할 때 상주 MCP를 시작합니다.")
	add("Resident MCP will no longer start automatically at sign-in.", "로그인할 때 상주 MCP를 자동으로 시작하지 않습니다.")
	add("Login startup settings were not changed.", "로그인 시작 설정을 변경하지 않았습니다.")
	add("A different login startup entry uses the reserved name. It was left unchanged.", "예약된 로그인 시작 이름에 다른 항목이 있습니다. 해당 항목은 변경하지 않았습니다.")
	add("Could not update Windows login startup. Existing settings were left unchanged.", "Windows 로그인 시작을 변경하지 못했습니다. 기존 설정은 그대로 유지됩니다.")
	add("Python task was not saved. Check the name and absolute Python/script paths.", "Python 작업을 저장하지 못했습니다. 이름과 Python 실행 파일·스크립트의 절대 경로를 확인하세요.")
	add("Python task was not saved. Check the task details and secret mapping.", "Python 작업을 저장하지 못했습니다. 작업 정보와 비밀값 매핑을 확인하세요.")
	add("Python task was not saved. Confirm the run-as-user notice.", "Python 작업을 저장하지 못했습니다. 현재 사용자 권한으로 실행된다는 안내를 확인하세요.")
	add("Python task was not saved. Review and confirm the current executable and script files before saving.", "Python 작업을 저장하지 못했습니다. 현재 실행 파일과 스크립트를 검토하고 확인한 뒤 저장하세요.")
	add("Python task was not saved. The executable or script could not be read within the allowed size limits.", "Python 작업을 저장하지 못했습니다. 실행 파일 또는 스크립트를 허용된 크기 안에서 읽지 못했습니다.")
	add("Python task saved.", "Python 작업을 저장했습니다.")
	add("Could not save Python task settings. Previous settings are unchanged, but an unused credential remains in Windows Credential Manager. Remove the Local Agent Harness credential entry before retrying.", "Python 작업 설정을 저장하지 못했습니다. 이전 설정은 그대로지만 Windows Credential Manager에 사용하지 않는 자격 증명이 남아 있습니다. 다시 시도하기 전에 Local Agent Harness 자격 증명 항목을 제거하세요.")
	add("Python task saved, but the previous unused credential could not be removed from Windows Credential Manager.", "Python 작업은 저장했지만 이전의 미사용 자격 증명을 Windows Credential Manager에서 삭제하지 못했습니다.")
	add("Not found", "찾을 수 없습니다.")
	add("Method not allowed", "허용되지 않은 요청 방식입니다.")
	add("Request origin rejected", "요청 출처가 허용되지 않았습니다.")
	add("Request rejected", "요청이 거부되었습니다.")
	add("Could not render confirmation page", "확인 화면을 표시할 수 없습니다.")
	add("Invalid confirmation request", "확인 요청이 올바르지 않습니다.")
	add("Confirmation rejected", "확인 요청이 거부되었습니다.")
	add("Invalid confirmation decision", "확인 선택이 올바르지 않습니다.")
	add("Confirmation already used", "이미 처리된 확인 요청입니다.")
	add("Approval recorded. You may close this page.", "승인을 기록했습니다. 이 창을 닫아도 됩니다.")
	add("Request denied. You may close this page.", "요청을 거부했습니다. 이 창을 닫아도 됩니다.")
	add(
		"Could not read local settings. Check the settings file and restart the app.",
		"로컬 설정을 읽을 수 없습니다. 설정 파일을 확인하고 앱을 다시 시작하세요.",
	)
	add("Correct the highlighted Harbor field before saving.", "저장하기 전에 강조된 Harbor 항목을 수정하세요.")
	add("Correct the highlighted Dashboard field before saving.", "저장하기 전에 강조된 Dashboard 항목을 수정하세요.")
	add(
		"Enter an HTTPS origin such as https://github.example.invalid. Paths, query strings, and credentials are not accepted.",
		"https://github.example.invalid 같은 HTTPS 원본 주소를 입력하세요. 경로, 쿼리 문자열, 자격 증명은 사용할 수 없습니다.",
	)
	add(
		"Enter one repository as owner/name using letters, numbers, dots, underscores, or hyphens.",
		"저장소 하나를 owner/name 형식으로 입력하세요. 영문자, 숫자, 마침표, 밑줄, 하이픈을 사용할 수 있습니다.",
	)
	add("Enter a target name up to 80 characters.", "대상 이름을 80자 이내로 입력하세요.")
	add("The personal access token is invalid or too long.", "개인 액세스 토큰이 올바르지 않거나 너무 깁니다.")
	add(
		"Enter a valid Jenkins name, HTTPS base URL, username, and job path.",
		"올바른 Jenkins 이름, HTTPS 기본 URL, 사용자 이름, 작업 경로를 입력하세요.",
	)
	add("The Jenkins API token is invalid or too long.", "Jenkins API 토큰이 올바르지 않거나 너무 깁니다.")
	add(
		"GitHub target saved. Test the connection before using MCP.",
		"GitHub 대상을 저장했습니다. MCP에서 사용하기 전에 연결을 테스트하세요.",
	)
	add(
		"Jenkins target saved. Test the connection before using MCP.",
		"Jenkins 대상을 저장했습니다. MCP에서 사용하기 전에 연결을 테스트하세요.",
	)
	add(
		"Harbor target saved. Test connection before using MCP.",
		"Harbor 대상을 저장했습니다. MCP에서 사용하기 전에 연결을 테스트하세요.",
	)
	add(
		"Dashboard target saved. Test connection before using MCP.",
		"Dashboard 대상을 저장했습니다. MCP에서 사용하기 전에 연결을 테스트하세요.",
	)
	add(
		"Connection test was not run. Check the saved target and credential settings.",
		"연결 테스트를 실행하지 않았습니다. 저장된 대상과 자격 증명 설정을 확인하세요.",
	)
	add(
		"Connection test succeeded, but its history could not be saved. Retest the current target.",
		"연결 테스트는 성공했지만 이력을 저장하지 못했습니다. 현재 대상을 다시 테스트하세요.",
	)
	add("Connection test succeeded. Historical status saved.", "연결 테스트에 성공했고 이력에 결과를 저장했습니다.")
	add("The Harbor name field is invalid.", "Harbor 이름 항목이 올바르지 않습니다.")
	add("The Harbor url field is invalid.", "Harbor URL 항목이 올바르지 않습니다.")
	add("The Harbor username field is invalid.", "Harbor 사용자 이름 항목이 올바르지 않습니다.")
	add("The Harbor project field is invalid.", "Harbor 프로젝트 항목이 올바르지 않습니다.")
	add("The Harbor repository field is invalid.", "Harbor 저장소 항목이 올바르지 않습니다.")
	add("The Harbor secret field is invalid.", "Harbor 자격 증명 항목이 올바르지 않습니다.")
	add(
		"The Dashboard name field is invalid. A connection test was not run.",
		"Dashboard 이름 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.",
	)
	add(
		"The Dashboard url field is invalid. A connection test was not run.",
		"Dashboard URL 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.",
	)
	add(
		"The Dashboard token field is invalid. A connection test was not run.",
		"Dashboard 토큰 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.",
	)
	add("Target availability updated.", "대상의 사용 가능 상태를 변경했습니다.")
	add("Check the delete confirmation before removing this target.", "대상을 삭제하기 전에 삭제 확인 항목을 선택하세요.")
	add("Target deleted from local settings.", "로컬 설정에서 대상을 삭제했습니다.")
	add("Service bundle saved.", "서비스 묶음을 저장했습니다.")
	add("Confirm service bundle deletion before removing it.", "서비스 묶음을 삭제하기 전에 삭제를 확인하세요.")
	add("Service bundle deleted.", "서비스 묶음을 삭제했습니다.")
	add("Could not read current settings. They were not changed.", "현재 설정을 읽을 수 없습니다. 설정은 변경되지 않았습니다.")
	add("Could not read current settings.", "현재 설정을 읽을 수 없습니다.")
	add("Could not create target ID. Settings were not changed.", "대상 ID를 만들 수 없습니다. 설정은 변경되지 않았습니다.")
	add(
		"Could not create credential reference. Settings were not changed.",
		"자격 증명 참조를 만들 수 없습니다. 설정은 변경되지 않았습니다.",
	)
	add(
		"Saving secrets is available only on Windows with Credential Manager. No secret was saved.",
		"비밀값 저장은 Windows Credential Manager에서만 사용할 수 있습니다. 비밀값은 저장되지 않았습니다.",
	)
	add(
		"Could not save the credential in Windows Credential Manager. Settings were not changed.",
		"Windows Credential Manager에 자격 증명을 저장할 수 없습니다. 설정은 변경되지 않았습니다.",
	)
	add(
		"Could not save settings. Previous settings are unchanged, but an unused credential remains in Windows Credential Manager. Remove the Local Agent Harness credential entry before retrying.",
		"설정을 저장하지 못했습니다. 이전 설정은 그대로지만 Windows Credential Manager에 사용하지 않는 자격 증명이 남아 있습니다. 다시 시도하기 전에 Local Agent Harness 자격 증명 항목을 제거하세요.",
	)
	add(
		"Could not save settings. Previous settings are unchanged, but an unused credential remains in Windows Credential Manager.",
		"설정을 저장하지 못했습니다. 이전 설정은 그대로지만 Windows Credential Manager에 사용하지 않는 자격 증명이 남아 있습니다.",
	)
	add("Could not save settings. Previous settings remain in place.", "설정을 저장하지 못했습니다. 이전 설정은 그대로 유지됩니다.")
	add("The selected GitHub target is not registered.", "선택한 GitHub 대상이 등록되어 있지 않습니다.")
	add("The selected Jenkins target is not registered.", "선택한 Jenkins 대상이 등록되어 있지 않습니다.")
	add("The selected Harbor target is not registered.", "선택한 Harbor 대상이 등록되어 있지 않습니다.")
	add("The selected Dashboard target is not registered.", "선택한 Dashboard 대상이 등록되어 있지 않습니다.")
	add("A GitHub target with that name is already registered.", "같은 이름의 GitHub 대상이 이미 등록되어 있습니다.")
	add("A Jenkins target with that name is already registered.", "같은 이름의 Jenkins 대상이 이미 등록되어 있습니다.")
	add("A Harbor target name is already registered.", "같은 이름의 Harbor 대상이 이미 등록되어 있습니다.")
	add("A Dashboard target name is already registered.", "같은 이름의 Dashboard 대상이 이미 등록되어 있습니다.")
	add("Enter a personal access token to register a new target.", "새 대상을 등록하려면 개인 액세스 토큰을 입력하세요.")
	add("Enter a Jenkins API token to register a new target.", "새 대상을 등록하려면 Jenkins API 토큰을 입력하세요.")
	add("Enter a Harbor credential for the new target.", "새 대상을 등록하려면 Harbor 자격 증명을 입력하세요.")
	add(
		"Enter the Dashboard API Bearer returned by its login flow for a new target.",
		"새 대상을 등록하려면 Dashboard 로그인에서 발급한 API Bearer 토큰을 입력하세요.",
	)
	add(
		"Changing HTTPS origin requires entering a token for the new host. The saved credential was not sent.",
		"HTTPS 원본 주소를 바꾸려면 새 호스트의 토큰을 입력하세요. 저장된 자격 증명은 전송되지 않았습니다.",
	)
	add(
		"Changing Jenkins base URL requires entering a new API token. The saved credential was not sent.",
		"Jenkins 기본 URL을 바꾸려면 새 API 토큰을 입력하세요. 저장된 자격 증명은 전송되지 않았습니다.",
	)
	add(
		"Changing the Harbor base URL or username requires entering the new credential.",
		"Harbor 기본 URL이나 사용자 이름을 바꾸려면 새 자격 증명을 입력하세요.",
	)
	add(
		"Changing the Dashboard URL requires entering a new bearer token.",
		"Dashboard URL을 바꾸려면 새 Bearer 토큰을 입력하세요.",
	)
	add("The saved target is invalid. It was not changed.", "저장된 대상이 올바르지 않습니다. 대상은 변경되지 않았습니다.")
	add(
		"The saved Jenkins target is invalid. It was not changed.",
		"저장된 Jenkins 대상이 올바르지 않습니다. 대상은 변경되지 않았습니다.",
	)
	add(
		"The saved Harbor target is invalid. It was not changed.",
		"저장된 Harbor 대상이 올바르지 않습니다. 대상은 변경되지 않았습니다.",
	)
	add(
		"The saved Dashboard target is invalid. It was not changed.",
		"저장된 Dashboard 대상이 올바르지 않습니다. 대상은 변경되지 않았습니다.",
	)
	add("The GitHub target is invalid. Settings were not changed.", "GitHub 대상이 올바르지 않습니다. 설정은 변경되지 않았습니다.")
	add("The Jenkins target is invalid. Settings were not changed.", "Jenkins 대상이 올바르지 않습니다. 설정은 변경되지 않았습니다.")
	add("The Harbor target is invalid. Settings were not changed.", "Harbor 대상이 올바르지 않습니다. 설정은 변경되지 않았습니다.")
	add(
		"The Dashboard target is invalid. Settings were not changed.",
		"Dashboard 대상이 올바르지 않습니다. 설정은 변경되지 않았습니다.",
	)
	add("The Dashboard target name or bearer token is invalid.", "Dashboard 대상 이름이나 Bearer 토큰이 올바르지 않습니다.")
	add("The target is invalid. Settings were not changed.", "대상이 올바르지 않습니다. 설정은 변경되지 않았습니다.")
	add(
		"GitHub target saved, but the previous unused credential could not be removed.",
		"GitHub 대상을 저장했지만 이전에 사용하던 자격 증명을 제거하지 못했습니다.",
	)
	add(
		"Jenkins target saved, but the previous unused credential could not be removed.",
		"Jenkins 대상을 저장했지만 이전에 사용하던 자격 증명을 제거하지 못했습니다.",
	)
	add(
		"Harbor target saved, but its previous unused credential could not be removed.",
		"Harbor 대상을 저장했지만 이전에 사용하던 자격 증명을 제거하지 못했습니다.",
	)
	add(
		"Dashboard target saved, but its previous unused credential could not be removed.",
		"Dashboard 대상을 저장했지만 이전에 사용하던 자격 증명을 제거하지 못했습니다.",
	)
	add("Unknown target type.", "알 수 없는 대상 종류입니다.")
	add(
		"Could not update target availability. Settings remain unchanged.",
		"대상의 사용 가능 상태를 변경하지 못했습니다. 설정은 그대로 유지됩니다.",
	)
	add("Remove this target from its service bundle before deleting it.", "대상을 삭제하기 전에 서비스 묶음에서 제거하세요.")
	add("Could not delete target. Settings remain unchanged.", "대상을 삭제하지 못했습니다. 설정은 그대로 유지됩니다.")
	add(
		"Target was deleted from settings, but its unused credential could not be removed from Windows Credential Manager.",
		"설정에서 대상을 삭제했지만 Windows Credential Manager의 미사용 자격 증명은 제거하지 못했습니다.",
	)
	add("The selected service bundle is not registered.", "선택한 서비스 묶음이 등록되어 있지 않습니다.")
	add("Could not create service bundle ID.", "서비스 묶음 ID를 만들 수 없습니다.")
	add(
		"The service bundle is invalid or references unavailable targets. Settings were not changed.",
		"서비스 묶음이 올바르지 않거나 사용할 수 없는 대상을 참조합니다. 설정은 변경되지 않았습니다.",
	)
	add("Could not save service bundle. Settings remain unchanged.", "서비스 묶음을 저장하지 못했습니다. 설정은 그대로 유지됩니다.")
	add("Could not delete service bundle. Settings remain unchanged.", "서비스 묶음을 삭제하지 못했습니다. 설정은 그대로 유지됩니다.")
	return messages
}()
