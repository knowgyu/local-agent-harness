# MCP 클라이언트 설치·연결 안내

이 문서는 Windows에서 MCP stdio 프로세스를 수동 등록하는 예시를 제공합니다. 실제 클라이언트 설치·연결·도구 호출이나 서비스 호환성을 보증하지 않습니다. 공급자의 현재 문서를 확인한 뒤 각 클라이언트 버전에 맞게 사용하세요.

## 먼저 확인할 것

제품은 두 진입점을 제공한다.

- 일반 실행은 Windows 네이티브 UI를 연다. UI 주소는 `http://127.0.0.1:<임의 포트>/` 형태다.
- `local-agent-harness.exe --mcp`는 MCP stdio 서버를 실행한다. 클라이언트가 이 프로세스를 시작하므로 로컬 UI 주소를 MCP 서버 주소로 입력하지 않는다.

Go가 설치된 저장소 루트에서 실행 파일을 만들려면 다음과 같이 한다.

```powershell
go build -o .\local-agent-harness.exe .
(Resolve-Path .\local-agent-harness.exe).Path
```

클라이언트 등록 명령의 경로를 위에서 확인한 절대 경로로 바꾼다. UI에서 비밀값을 등록한 Windows 사용자와 MCP 클라이언트 프로세스의 Windows 사용자를 일치시킨다. Linux/WSL 실행은 이 Windows Credential Manager 경로를 사용하지 않는다. 등록 후 각 CLI의 상태 확인 명령을 실행한다. 이미 열린 CLI가 새 서버를 보이지 않으면 해당 클라이언트가 제공하는 reload/reconnect 기능을 쓰거나 새 세션을 연다. 설정에 등록됐다는 사실만으로 연결 성공을 뜻하지 않는다.

## Codex CLI

1. [Codex CLI 설치 안내](https://help.openai.com/en/articles/11096431)에 따라 설치하고 `codex --version`으로 버전을 기록한다. Windows npm 설치 명령은 `npm install -g @openai/codex`다.
2. PowerShell에서 exe 경로를 지정해 사용자 MCP 서버를 등록한다.

```powershell
codex mcp add local-agent-harness -- "C:\path\to\local-agent-harness.exe" --mcp
codex mcp list
```

`codex mcp list`는 저장된 서버 구성을 보여준다. Codex CLI 세션을 새로 열고 `/mcp`에서 활성 MCP 서버와 도구를 확인한다. Codex CLI, ChatGPT 데스크톱 앱, Codex IDE 확장은 같은 호스트에서 MCP 설정을 공유하므로 이 CLI 명령은 공유 설정을 갱신한다. 서버가 계속 표시되지 않으면 해당 클라이언트가 Restart 제어를 제공하는 경우 사용할 수 있다. 이 문서는 CLI 설치·확인 절차를 설명하며 별도의 데스크톱·IDE 설정 화면은 다루지 않는다. 등록 명령·공유 범위는 [공식 Codex MCP 안내](https://developers.openai.com/codex/mcp)를 참조한다.

## Claude Code CLI

1. PowerShell에서 [Claude Code 빠른 시작](https://code.claude.com/docs/en/quickstart)에 기재된 Windows 네이티브 설치를 실행한다. 설치 뒤 새 PowerShell 창을 열고 `claude --version`으로 버전을 기록한다.

```powershell
irm https://claude.ai/install.ps1 | iex
```
새 PowerShell 창에서 다음을 실행한다.

```powershell
claude --version
```
2. 사용자 범위에 stdio 서버를 등록한다.

```powershell
claude mcp add --scope user --transport stdio local-agent-harness -- "C:\path\to\local-agent-harness.exe" --mcp
claude mcp list
claude mcp get local-agent-harness
```

`claude mcp list`의 상태와 `claude mcp get local-agent-harness`를 확인하고, `claude` 세션의 `/mcp`에서 연결 및 도구를 확인한다. 서버가 보이지 않거나 연결에 실패하면 `/mcp`에서 상태를 확인한 다음 `claude mcp get local-agent-harness`를 실행한다. 그래도 해결되지 않으면 새 Claude Code CLI 세션을 연다. 옵션과 상태 의미는 [Claude Code MCP 안내](https://code.claude.com/docs/en/mcp)를 참조한다. 이 예시는 Claude Code CLI에 한정되며 Claude Desktop 연결을 뜻하지 않는다.

## Gemini CLI

1. 현재 공식 문서는 Windows 11 24H2 이상과 Node.js 20 이상을 명시한다. Node.js가 준비된 PowerShell에서 [Gemini CLI 설치 안내](https://geminicli.com/docs/get-started/installation/)의 npm 설치를 실행하고 `gemini --version`으로 버전을 기록한다.

```powershell
npm install -g @google/gemini-cli
gemini --version
```
2. 실제 실행 파일의 절대 경로를 지정해 사용자 범위에 stdio 서버를 등록한다.

```powershell
gemini mcp add --scope user local-agent-harness "C:\path\to\local-agent-harness.exe" -- --mcp
gemini mcp list
```

`C:\path\to\local-agent-harness.exe`를 실제 절대 경로로 바꾼다. `gemini mcp list`는 등록 및 연결 상태를 보여주며, 대화형 세션에서는 `/mcp list`로 확인할 수 있다. stdio 연결 여부는 현재 작업 폴더가 신뢰된 경우에만 연결 검사 후 `Connected`로 표시된다. `Disconnected`만 보이면 등록 파일보다 먼저 현재 폴더의 신뢰 상태를 확인하고, 폴더를 검토한 뒤 필요할 때 `gemini trust`를 실행한다. 설정 파일을 직접 편집할 경우에는 기존 항목을 보존하고 `mcpServers` 안에 병합한다. 자세한 구문과 재로드 명령은 [Gemini MCP 안내](https://geminicli.com/docs/tools/mcp-server/)를 참조한다.

## 페이지 내 사용자 보고 체크인

각 CLI 안내에는 아래 세 체크인이 별도로 제공된다. 각 항목은 사용자가 직접 확인한 내용만 표시하며 서로 독립적이다.

| CLI | 체크인 1 | 체크인 2 | 체크인 3 |
|---|---|---|---|
| Codex CLI | 등록 명령을 실행하고 저장된 MCP 목록에서 서버를 봤다고 보고 | 활성 Codex CLI 세션에서 이 서버의 MCP 도구를 봤다고 보고 | `registered_targets` 또는 `registered_service_bundles`를 호출해 결과를 봤다고 보고 |
| Claude Code CLI | 등록 명령을 실행하고 저장된 MCP 목록에서 서버를 봤다고 보고 | 활성 Claude Code CLI 세션에서 이 서버의 MCP 도구를 봤다고 보고 | `registered_targets` 또는 `registered_service_bundles`를 호출해 결과를 봤다고 보고 |
| Gemini CLI | 등록 명령을 실행하고 저장된 MCP 목록에서 서버를 봤다고 보고 | 활성 Gemini CLI 세션에서 이 서버의 MCP 도구를 봤다고 보고 | `registered_targets` 또는 `registered_service_bundles`를 호출해 결과를 봤다고 보고 |

선택 상태와 상태 문구는 해당 페이지의 메모리에만 머물며 앱 설정이나 클라이언트 설정 파일에 저장되지 않는다. 페이지를 새로고침하면 모두 초기화된다. 각 CLI 카드의 초기화 버튼은 그 CLI의 세 보고만 지운다. 제품은 체크인을 위해 Codex·Claude·Gemini 설정을 읽거나 연결을 검사하지 않는다. 문구는 사용자가 보고한 사실을 요약할 뿐, 실제 연결 또는 도구 호출을 앱이 확인했다는 의미가 아니다.

## 첫 확인과 주의점

연결 직후에는 `registered_targets` 또는 `registered_service_bundles`처럼 로컬 설정을 나열하는 catalog 도구만 확인한다. catalog 조회는 대상 서비스에 접속하지 않는다. 세 번째 페이지 체크인은 사용자가 이 CLI 세션에서 catalog 응답을 봤다고 보고하는 용도다. MCP 대상 연결 시험이 필요하면 `registered_target_connection_test`에 adapter와 정확한 등록 대상 이름만 지정한다. 유효하고 활성화된 대상과 credential이 있으면 저장된 주소로 제한된 GET 요청을 보내고 로컬 이력을 갱신할 수 있다. 요청이 시작되지 않으면 `not_tested`를 반환하고 `completed_at`은 이력 저장에 성공했을 때만 포함한다. 이 도구는 Codex/Claude/Gemini client process나 stdio 연결 상태를 확인하지 않는다. 다른 GitHub·Jenkins·Harbor·Dashboard 작업 도구도 등록된 실제 주소로 요청을 보낼 수 있으므로, 사용할 대상과 권한을 먼저 확인한다.

등록 해제는 각 클라이언트가 제공하는 MCP 서버 관리 명령이나 설정 화면을 이용한다. Gemini 설정을 수정할 때는 `settings.json`의 다른 사용자 값을 보존한다. 제품은 클라이언트 설정 파일을 자동으로 생성·수정·삭제하지 않는다.
