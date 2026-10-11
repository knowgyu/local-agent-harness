# Registration URL security review

Review date: 2026-10-11

## Result

No material actionable security issue was found in the scoped registration URL intake flow. The preview treats the submitted address as bounded text to parse; it does not resolve or contact that address. Invalid requests and URLs receive fixed error messages rather than the submitted value.

## Scope and evidence

Reviewed `registration_url.go`, `registration_url_ui.go`, their existing tests, the registration intake and candidate application code in `ui.html`, and the route registration in `local_ui_handler.go`. Traced shared controls in `main.go`, `mcp_http.go`, `git_import.go`, and `jenkins.go`.

- `registration_url_ui.go:27-98` applies the POST size/CSRF check, checks the configured Host and allowed Origin, accepts only the three supported services, and returns a candidate or fixed error DTO. `registration_url_ui.go:100-118` requires one URL-encoded form with exactly one each of `csrf`, `service`, and `url`; it rejects query parameters, multipart bodies, unknown fields, and duplicate fields. The response is `no-store`, `nosniff`, and JSON (`registration_url_ui.go:127-138`).
- `main.go:611-629` adds the local Host check and rejects mismatched or duplicate POST Origin headers. `main.go:1521-1539` bounds the parsed body and requires the CSRF value. The endpoint's explicit checks provide a second route-level check. Requests without an Origin still need the CSRF value.
- `registration_url.go:30-61` bounds URL length and rejects non-HTTPS URLs, userinfo, query strings, fragments, backslashes, whitespace, and unsupported services. `registration_url.go:185-219` decodes path components individually and rejects encoded separators, empty components, and dot traversal. Host, repository, Jenkins base URL, and job segments are then checked with the existing validators.
- The preview handler contains no outbound request, configuration read/write, or credential-store call. `TestHandleRegistrationURLPreviewDoesNotReadOrMutateLocalStateOrContactTargets` uses spies and a temporary config file to check that boundary. The malformed-request tests cover wrong Host/Origin/CSRF, duplicate and unknown fields, multipart, oversized bodies, and secret canaries in rejected URL forms.
- `ui.html:2741-2750` fetches only the same-origin preview route. `ui.html:2752-2759` discards responses whose generation or submitted values are stale. Candidate values and switch-message names are inserted with `textContent` (`ui.html:2662-2673`, `ui.html:2783-2795`), avoiding HTML interpretation.
- Existing-target application is guarded in `ui.html:2889-2930` and `ui.html:2979-2999`: a candidate for an open saved target requires a separate confirmation, and the confirmation checks that the open target and picker selection have not changed. The confirmed transition clears only the in-page form before applying candidate fields; this path does not submit a save request. This was verified by source tracing, not by browser automation.

## Verification and limits

Ran the existing focused Go tests with `go test -run 'Test(ParseRegistrationURL|HandleRegistrationURLPreview)' .`; they passed. No test file or production file was changed for this review.

The review did not run a live browser exercise of out-of-order responses or existing-target switching, contact real services, use real credentials, or validate network behavior after a user saves a target. The preview's no-contact conclusion is limited to this handler and its tests. Path components are treated as resource data; the UI warns users not to include tokens, but the parser cannot determine whether an otherwise valid path segment is a secret.
