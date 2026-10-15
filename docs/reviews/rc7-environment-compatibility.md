# RC7 environment compatibility

## Scope

Public GitHub API routing, saved Enterprise hosts/ports, Jenkins context paths and ordinary job names, bounded request timeouts, and environment recovery guidance. No new transport, discovery scan, authentication provider, retry policy or dependency.

## Changes

- The repository URL builder maps only the exact public GitHub hosts on the default HTTPS port to `api.github.com`. Enterprise hosts, lookalike hosts and nonstandard ports keep their saved authority and `/api/v3`. Initial GitHub checks reuse that builder.
- Jenkins accepts ordinary Unicode letters/marks/numbers, ASCII space, parentheses and `+` in job segments. URLs store decoded path components and encode once on serialization. Initial checks reuse the action URL builder. Context-path validation remains canonical; encoded separators, traversal, controls and invalid UTF-8 remain blocked.
- Service calls share a 30-second bound. The local UI response timeout covers that bound. Caller cancellation, output caps, redirect restrictions, secret masking and ambiguous-trigger handling remain in place.
- Korean usage and English agent instructions describe native Windows network prerequisites and target-specific recovery. They do not promise PAC, integrated proxy authentication, browser SSO or arbitrary API prefixes.

## Evidence

The new compatibility cases failed at their intended endpoint/name boundary before the fixes and passed afterward on WSL Go 1.26.7. Fresh native and package results belong in the RC7 validation summary. Review the exact published assets before claiming a release gate passed.

## Limits

Synthetic adapters do not prove company connectivity, trusted-root deployment, real vault operations, CLI/Desktop hosting or human browser acceptance. No TLS bypass or unknown-host fallback was added.

## Independent review and release checks

The independent review found an equivalent-port case (`0443`) and three documentation defects. Public HTTPS port matching now accepts validated leading-zero spellings. The index points to the RC7 review, PRD punctuation is corrected, and the Korean guide limits its space/Unicode claim to Jenkins job segments rather than base context paths.

Final native focused tests passed (9.28s), the full suite passed (37.82s), and vet passed (1.10s). The final documented portable package passed build/audit (21.48s); MCPB validation/packing/info passed (13.45s). Its EXE is identical to the final ZIP and isolated native stdio probe. The final reviewed ZIP passed 12/12 lifecycle workflows across Windows PowerShell 5.1.26100.9711 and PowerShell 7.6.7. Each runtime passed 9 recovery and 3 legacy-journal cases; RC4-named legacy arguments were pinned to current RC7, so this is RC3-to-RC7 compatibility. Separate public RC6-to-RC7 upgrade checks passed on both runtimes with exact payload/receipt and marker preservation. A task-runner module-environment failure preceded the corrected full rerun and is not counted as a product pass.

Verdict: Ship. Real company and client-host acceptance remain unverified.
