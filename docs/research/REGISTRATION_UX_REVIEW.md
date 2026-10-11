# First-wave registration UX review

Review date: 2026-10-11. Scope: the incumbent URL-first registration interface in `ui.html`, as shown by the saved actual Dashboard-candidate captures. This is a source-and-capture review of the first UI wave; it does not assess the parallel draft, pre-save check, policy, or receipt UI as those screens were not present in the reviewed captures.

## Evidence and method

Reviewed `PRODUCT.md`, the registration design brief, UX research and decision records, the implementation record, the actual UI finish review, and the current `ui.html` registration markup and handoff code. Opened these saved captures:

| Capture | Observed state |
| --- | --- |
| `actual-candidate-desktop-1440.jpg` | 1440 CSS px, full page; Dashboard address candidate and all service sections in view |
| `actual-candidate-mobile-390.jpg` | 390 CSS px, full page; single-column address form, candidate, operation cards, and service sections |
| `actual-candidate-user-1033.jpg` | 1033 CSS px, top-of-document viewport; Dashboard candidate visible, lower content available by scrolling |

No browser was opened and no new detector or interaction run was performed for this independent review. The captures are the actual first-wave UI evidence and the finish review records no horizontal overflow or console errors. A focused local UX database query returned applicable guidance to associate visible labels with inputs and provide submit feedback; this flow does both through explicit labels and an inline live status.

## Assessment

The first-wave flow is careful about truth and user control. It presents a service selector, HTTPS address input, explicit local address check, reviewable candidate, and explicit apply action. Its copy consistently says the preview parses URL shape locally; it neither contacts the service nor saves settings, credentials, or connection state. The source then opens the matching provider section, reveals its new-target and authentication guidance, scrolls to it, and focuses the target-name field. This is a sound, understandable handoff.

The strongest remaining friction is the order of content after the candidate: three operation cards appear before the provider target forms. At 390 px they become three extra stacked cards between “apply values” and the form the user needs next. The address flow is a clear next step, but the operation cards compete for attention before the guided setup is complete.

### Heuristic snapshot

Scores use 0 (serious barrier) to 4 (strong). They assess only the observed first-wave registration surface.

| Heuristic | Score | Evidence |
| --- | ---: | --- |
| Visibility of system status | 3 | Inline live status and candidate review; the preview remains clearly separate from service connectivity. |
| Match with real-world language | 3 | Service names and familiar URLs lead the flow; some technical authentication terms remain in vendor help. |
| User control and freedom | 3 | User explicitly checks, reviews, applies, and confirms a new-target switch; there is no durable draft yet in this wave. |
| Consistency and standards | 3 | Native labeled controls, disclosure sections, familiar save/test actions, and visible keyboard focus continue the incumbent UI. |
| Error prevention | 4 | Secret-in-URL warning, unsupported query/fragment handling, explicit candidate review, and separate apply/save/test actions guard consequential mistakes. |
| Recognition rather than recall | 3 | URL-derived missing fields and service-specific required fields are listed; URL does not have to be manually split first. |
| Flexibility and efficiency | 3 | Same entry supports three requested URL-backed services while existing edit forms remain available; 1–5 target scale fits the restrained layout. |
| Aesthetic and minimalist design | 2 | Incumbent visual language is coherent, but operation cards interrupt the setup sequence and add mobile scroll before the target form. |
| Error recovery | 3 | Invalid input has inline status, changed URL/service invalidates the candidate, and current target values are protected by a confirmation step. |
| Help and documentation | 3 | Authentication and operation-boundary help is available in the matching service form; help appears after applying the candidate. |
| **Total** | **30/40** | Strong truth and safety cues; improve the sequence between candidate review and target completion. |

There are no more than four service choices in the primary flow. The broader settings navigation is grouped into Settings and Operations; the saved captures do not show horizontal scrolling or mobile form overflow.

## Priority finding

**[P2] Operation cards interrupt new-target setup.** The three `lah-quick-action` cards for Dashboard diagnosis, SSH, and Python tasks are rendered after the URL-candidate section and before the provider target panels (`ui.html`, lines 360–363). They remain visible in the empty-target state. The operation cards are useful destinations, but at the point where a first-time user has just reviewed a candidate, they break the natural URL → apply → complete-target sequence. This conflicts with the research observation and recommendation to put first connection ahead of diagnosis/SSH/Python cards when no targets exist (`UX_RESEARCH_2026-10-10.md`, line 38). The 390 px capture makes the cost most visible because the cards stack vertically.

**Bounded fix:** retain Operations in the main navigation, and move the quick cards below the provider target setup sections (or collapse them under an Operations disclosure there). This preserves setup and operations access while letting the registration flow finish before unrelated shortcuts.

Status: the integration owner reported that this exact move is authorized and in progress; this review records the captured first-wave baseline and does not claim the updated layout has been recaptured.

## Strengths and material gaps

- Clear evidence boundary: URL parsing, applying values, saving, connection testing, and an AI app’s first read are presented as distinct actions. No false service-success state appears in the captured candidate.
- Good continuation affordance: applying values opens the matching target panel and moves focus to the target name. Vendor help is in a disclosure instead of taking over the initial URL step.
- The current first-wave capture does not show the general draft, pre-save service check, policy review, or first-read receipt. The implementation plan records these as later work, and the parallel wave is underway. Review those as new states when available; do not interpret the first-wave SHIP visual disposition as evidence they already exist.
- The button and status use “주소 확인” (“check address”); nearby copy repeatedly limits this to local URL-shape parsing. A more specific label such as “주소에서 후보 찾기” would be marginally harder to mistake for a connectivity check, but the current copy prevents a material false-success claim.

## Conclusion

The URL-first intake is accurate, safe, and usable at the reviewed desktop, 390 px mobile, and 1033 px user viewport sizes. The saved actual UI finish review’s SHIP disposition remains consistent for its visual scope. The main usability improvement identified here is the operation-card ordering; that bounded fix is underway and still needs a fresh capture before it can be considered reviewed.

## Expanded onboarding wave: source-only review

Review date: 2026-10-11. This is a separate scope from the captured first-wave review above. I read the expanded `ui.html` draft/check/policy/activation and AI-client guide paths, plus the onboarding implementation contract, verification snapshot, and Desktop compatibility note. Root has reserved the new browser run and actual captures; this review makes no visual pass claim. The operation-card move is present in source after provider setup and the onboarding panel, but also awaits those captures.

### Material findings

**[P1] The fixed-scope confirmation lists internal tool identifiers without explaining their effects.** The review panel renders `included_tools`, `excluded_tools`, and `mapping_required` as plain text (`ui.html`, around line 3621); the backend values include names such as `github_repository`, `github_pull_request`, and `dashboard_deployment_pod_logs` (`onboarding_activation.go`, lines 29–42). For a Korean-first operator, these identifiers do not say what data will be read. The adjacent statement that individual tools cannot be selected makes understanding the list more important: the user can approve a whole service scope, but cannot narrow it. Add a concise Korean purpose for each tool or capability, and retain the MCP identifier as secondary detail if useful. Keep mapping-dependent Dashboard reads clearly separate from the namespace read actually included.

**[P2] The ordinary draft resume route is easy to confuse with AI-generated proposal review.** The Settings and Operations navigation labels a pane “AI 설정 초안” (`ui.html`, line 304), but that pane links to the legacy AI proposal-review page (`ui.html`, lines 442–449). The new user-authored registration drafts are listed in the “연결 설정 이어하기” block inside Connections (`ui.html`, lines 379–412). A user returning to continue an interrupted service setup can reasonably choose the similarly named sidebar route and land in the wrong workflow. Rename the existing route to “AI 제안 검토” and make “연결 설정 이어하기” directly reachable from Connections/Settings navigation. The `Open` action then resumes the ordinary draft and correctly clears credentials.

The Save progress control also needs an inline pre-action disclosure. The form’s nearby credential hint says the value is stored in Windows Credential Manager for an ordinary target save, while the warning that onboarding drafts omit the token and pasted URL is in the separate continuation panel (`ui.html`, lines 381 and 558/661/841). After the save, status text explains the omission, but that arrives after the user has chosen the action. Add a short hint beside each Save progress button: only normalized non-secret fields are saved; the raw pasted address and credentials must be supplied again. Do not store secrets to solve the discoverability issue.

**[P2] The new-target form still offers immediate target saving beside the guided draft path, without explaining the difference.** GitHub, Jenkins, and Dashboard forms keep the primary “Save … target” submit beside “Save progress” (`ui.html`, lines 558, 661, and 841). The guided path later requires a successful temporary check, fixed-scope review, and explicit activation, while the existing direct-save action can still create an active target without those steps. This preserves the manual path, but the two actions do not tell a first-time user what they are choosing between. The continuation panel’s broad sentence that a connection “is saved only after” the guided gates also overstates the guarantee while direct save remains available (`ui.html`, line 381). Label the direct-save path as immediate/legacy and say it skips the guided check and scope review, or otherwise narrow that sentence to the guided-draft path. Preserve direct manual saving if it remains an intended supported route.

**[P2] The post-activation handoff stops at client setup and readiness, without a first-read example.** Activation correctly says it did not run a connection test or AI-app read and links to the target test, the client guide, and Readiness (`ui.html`, lines 3714–3730). The client guide covers registration and `/mcp`/catalog checks, while carefully saying that a catalog call is local and does not contact a saved service (`ui.html`, lines 1104–1124). It does not show how to request one bounded external read for the just-registered GitHub repository, Jenkins job, or Dashboard resource. Since success is the first service read, add a localized read-only example for each supported service, clearly separate from the local catalog check. Keep it generic enough to avoid claiming that ChatGPT Desktop or Claude Desktop is installed or verified.

### What the source gets right

- A resumed draft shows its saved non-secret fields, clearly says credentials were cleared, focuses a missing required field or credential, and opens vendor-specific authentication help. Reopening a draft with conflicting unsaved form values offers an explicit keep-current-form choice.
- Changing a draft field or credential invalidates the in-memory check permit. A successful service check explicitly says it is not an AI-app read; policy review and user confirmation precede guided activation.
- Activation status and links separate target saving, the existing connection test, client setup, and observed first-read status. The Dashboard warning does not infer an auth mechanism from the visible version.
- Desktop scope is honestly described: the ChatGPT Desktop instructions are same-host/manual and do not claim a connection was verified; Claude Desktop is explicitly unsupported by a verified package here; Claude Code CLI is identified as a separate product path.

### Review boundary

This source review identified four material findings plus the pre-save draft disclosure. I did not exercise any action or inspect a new capture. The expanded wave remains **visually unreviewed** until the root-provided desktop/mobile/user captures and live browser run are assessed. No expanded-wave SHIP verdict is issued here.

### Bounded source recheck after UI-owner edits

Review date: 2026-10-11. Re-read the changed `ui.html` paths only; no browser, capture, action, or visual verdict was part of this check.

The previous source findings are addressed in the current markup and client code: the tool list leads with localized capability descriptions and leaves MCP IDs as secondary detail; an empty excluded-tools section is hidden; new-target forms make guided progress saving primary while preserving immediate save inside an explicit no-check disclosure; the save hint is adjacent to the action and names the normalized address and omitted credentials; the sidebar distinguishes AI proposal review from connection drafts; and successful activation offers a service-specific read-only request with a copy button, manual-copy fallback, and copy-only boundary text. The `#lah-onboarding` destination resolves through its enclosing Connections pane.

The check receipt must carry a positive safe-integer `check_generation`. The client binds policy review to the captured generation and receipt digests, then sends that generation with activation and requires the activation response to echo it. I found no material regression in these bounded changes. This confirms source wiring only; the expanded onboarding wave remains **visually unreviewed** pending the separate current desktop, mobile, and user-viewport captures/live review.
