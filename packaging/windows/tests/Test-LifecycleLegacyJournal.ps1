[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RC3PackageRoot,
    [Parameter(Mandatory)][string]$RC3PackageArchivePath,
    [Parameter(Mandatory)][string]$ExpectedRC3ArchiveSha256,
    [Parameter(Mandatory)][string]$RC4PackageRoot,
    [Parameter(Mandatory)][string]$RC4PackageArchivePath,
    [Parameter(Mandatory)][string]$ExpectedRC4ArchiveSha256,
    [Parameter(Mandatory)][string]$OutputRoot
)

<#
.SYNOPSIS
Verifies bounded recovery compatibility for an interrupted RC3 lifecycle journal.

.DESCRIPTION
Uses only synthetic install roots below OutputRoot. The two supplied package roots
must be flat extractions of the archives identified by the supplied SHA-256 pins.
No real installation root is read or changed.

.EXAMPLE
.\Test-LifecycleLegacyJournal.ps1 -RC3PackageRoot <rc3-root> -RC3PackageArchivePath <rc3.zip> `
  -ExpectedRC3ArchiveSha256 <sha256> -RC4PackageRoot <rc4-root> -RC4PackageArchivePath <rc4.zip> `
  -ExpectedRC4ArchiveSha256 <sha256> -OutputRoot <short-test-output-root>
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-Hash([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Assert-PackageBinding([string]$PackageRoot, [string]$ArchivePath, [string]$ExpectedSha256, [string[]]$ExpectedNames) {
    if ($ExpectedSha256 -cnotmatch '^[0-9a-fA-F]{64}$') { throw 'A package archive pin is malformed.' }
    if (-not (Test-Path -LiteralPath $PackageRoot -PathType Container) -or -not (Test-Path -LiteralPath $ArchivePath -PathType Leaf)) {
        throw 'A pinned package root or archive is missing.'
    }
    if ((Get-Hash $ArchivePath) -cne $ExpectedSha256.ToLowerInvariant()) { throw 'A package archive does not match its supplied SHA-256 pin.' }

    $rootFiles = @(Get-ChildItem -LiteralPath $PackageRoot -Force -File)
    $rootDirectories = @(Get-ChildItem -LiteralPath $PackageRoot -Force -Directory)
    if ($rootFiles.Count -ne $ExpectedNames.Count -or $rootDirectories.Count -ne 0) { throw 'A package root is not the expected flat payload.' }
    foreach ($file in $rootFiles) { if ($ExpectedNames -cnotcontains $file.Name) { throw 'A package root contains an unexpected member.' } }

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archive = [IO.Compression.ZipFile]::OpenRead($ArchivePath)
    $archiveHashes = @{}
    try {
        if ($archive.Entries.Count -ne $ExpectedNames.Count) { throw 'A pinned archive has the wrong member count.' }
        foreach ($entry in $archive.Entries) {
            $name = [string]$entry.FullName
            if ($ExpectedNames -cnotcontains $name -or $name.Contains('/') -or $name.Contains([char]92) -or $archiveHashes.ContainsKey($name)) {
                throw 'A pinned archive has an unexpected or repeated member.'
            }
            $stream = $entry.Open()
            $sha = [Security.Cryptography.SHA256]::Create()
            try { $archiveHashes[$name] = [BitConverter]::ToString($sha.ComputeHash($stream)).Replace('-', '').ToLowerInvariant() }
            finally { $stream.Dispose(); $sha.Dispose() }
        }
    } finally { $archive.Dispose() }

    foreach ($name in $ExpectedNames) {
        $path = Join-Path $PackageRoot $name
        if (-not $archiveHashes.ContainsKey($name) -or (Get-Hash $path) -cne $archiveHashes[$name]) {
            throw "An extracted package member does not match its pinned archive: $name"
        }
    }
}

function Update-PackageVariant([string]$SourceRoot, [string]$Destination) {
    [IO.Directory]::CreateDirectory($Destination) | Out-Null
    foreach ($file in @(Get-ChildItem -LiteralPath $SourceRoot -Force -File)) {
        [IO.File]::Copy($file.FullName, (Join-Path $Destination $file.Name), $false)
    }
    $readmePath = Join-Path $Destination 'README.md'
    [IO.File]::AppendAllText($readmePath, "`nSynthetic lifecycle journal regression variant.`n", (New-Object Text.UTF8Encoding($false)))
    $manifestPath = Join-Path $Destination 'manifest.json'
    $manifest = [IO.File]::ReadAllText($manifestPath) | ConvertFrom-Json
    foreach ($entry in @($manifest.files)) {
        $path = Join-Path $Destination ([string]$entry.path)
        $entry.size = [long](Get-Item -LiteralPath $path -Force).Length
        $entry.sha256 = Get-Hash $path
    }
    [IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding($false)))
    $sumLines = @(
        Get-ChildItem -LiteralPath $Destination -Force -File |
            Where-Object { $_.Name -cne 'SHA256SUMS.txt' } |
            Sort-Object Name |
            ForEach-Object { "$(Get-Hash $_.FullName)  $($_.Name)" }
    )
    [IO.File]::WriteAllLines((Join-Path $Destination 'SHA256SUMS.txt'), [string[]]$sumLines, (New-Object Text.UTF8Encoding($false)))
}

function New-InstrumentedScripts([string]$PackageRoot, [string]$Destination) {
    [IO.Directory]::CreateDirectory($Destination) | Out-Null
    foreach ($name in @('Install-LocalAgentHarness.ps1','Update-LocalAgentHarness.ps1','Uninstall-LocalAgentHarness.ps1')) {
        [IO.File]::Copy((Join-Path $PackageRoot $name), (Join-Path $Destination $name), $false)
    }
    $modulePath = Join-Path $PackageRoot 'LocalAgentHarness.Lifecycle.psm1'
    $moduleText = [IO.File]::ReadAllText($modulePath)
    $anchor = '        foreach ($name in $oldPresent) { [IO.File]::Move((Join-Path $root $name), (Join-Path $backupRoot $name)) }'
    $replacement = @'
        $script:LAHTestBarrierUsed = $false
        foreach ($name in $oldPresent) {
            [IO.File]::Move((Join-Path $root $name), (Join-Path $backupRoot $name))
            if (-not $script:LAHTestBarrierUsed) { $script:LAHTestBarrierUsed = $true; Invoke-LAHTestBarrier }
        }
'@
    if (-not $moduleText.Contains($anchor) -or $moduleText.IndexOf($anchor, [StringComparison]::Ordinal) -ne $moduleText.LastIndexOf($anchor, [StringComparison]::Ordinal)) {
        throw 'The deterministic update barrier did not match the pinned lifecycle source.'
    }
    $hook = @'
function Invoke-LAHTestBarrier {
    $hookRoot = Join-Path $PSScriptRoot '.test-hooks'
    [IO.Directory]::CreateDirectory($hookRoot) | Out-Null
    [IO.File]::WriteAllText((Join-Path $hookRoot 'update-first-backup.ready'), 'ready')
    while (-not (Test-Path -LiteralPath (Join-Path $hookRoot 'update-first-backup.continue'))) { Start-Sleep -Milliseconds 25 }
}
'@
    $moduleText = $moduleText.Replace($anchor, $replacement).Replace('Set-StrictMode -Version Latest', "Set-StrictMode -Version Latest`r`n$hook")
    [IO.File]::WriteAllText((Join-Path $Destination 'LocalAgentHarness.Lifecycle.psm1'), $moduleText, (New-Object Text.UTF8Encoding($false)))
    [IO.Directory]::CreateDirectory((Join-Path $Destination '.test-hooks')) | Out-Null
}

function Start-InterruptedUpdate([string]$ScriptsRoot, [string]$PackageRoot, [string]$InstallRoot, [string]$CaseRoot) {
    $scriptPath = Join-Path $CaseRoot 'update-child.ps1'
    $escapedPackage = $PackageRoot.Replace("'", "''")
    $escapedRoot = $InstallRoot.Replace("'", "''")
    $escapedScripts = $ScriptsRoot.Replace("'", "''")
    $childBody = "`$ErrorActionPreference='Stop'; & '$escapedScripts\Update-LocalAgentHarness.ps1' -PackageRoot '$escapedPackage' -InstallRoot '$escapedRoot'"
    [IO.File]::WriteAllText($scriptPath, $childBody, (New-Object Text.UTF8Encoding($false)))
    $signal = Join-Path (Join-Path $ScriptsRoot '.test-hooks') 'update-first-backup.ready'
    $stdout = Join-Path $CaseRoot 'child.stdout.txt'
    $stderr = Join-Path $CaseRoot 'child.stderr.txt'
    $exe = Join-Path $PSHOME 'powershell.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { $exe = Join-Path $PSHOME 'pwsh.exe' }
    $process = Start-Process -FilePath $exe -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File `"$scriptPath`"" -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $deadline = [DateTime]::UtcNow.AddSeconds(45)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $signal -PathType Leaf) {
            try { Stop-Process -Id $process.Id -Force -ErrorAction Stop } catch { }
            $process.WaitForExit()
            return
        }
        if ($process.HasExited) { throw 'A synthetic child exited before reaching its lifecycle barrier.' }
        Start-Sleep -Milliseconds 25
    }
    try { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue } catch { }
    throw 'A synthetic child did not reach its lifecycle barrier within 45 seconds.'
}

function Get-TransactionRoot([string]$InstallRoot) {
    $roots = @(Get-ChildItem -LiteralPath $InstallRoot -Force -Directory | Where-Object { $_.Name -cmatch '^\.lah-transaction-[0-9a-f]{32}$' })
    if ($roots.Count -ne 1) { throw 'An interrupted fixture did not leave exactly one transaction.' }
    return $roots[0].FullName
}

function Get-FixtureSnapshot([string]$Root) {
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    $snapshot = @{}
    foreach ($item in @(Get-ChildItem -LiteralPath $fullRoot -Force -Recurse)) {
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'A synthetic test fixture unexpectedly contains a reparse point.' }
        $relative = $item.FullName.Substring($fullRoot.Length).TrimStart('\')
        if ($item.PSIsContainer) { $snapshot[$relative] = 'directory' }
        else { $snapshot[$relative] = ('file|{0}|{1}' -f [long]$item.Length, (Get-Hash $item.FullName)) }
    }
    return $snapshot
}

function Assert-SnapshotEqual([hashtable]$Before, [hashtable]$After) {
    if ($Before.Count -ne $After.Count) { throw 'A rejected legacy journal changed the synthetic install tree.' }
    foreach ($path in $Before.Keys) {
        if (-not $After.ContainsKey($path) -or $After[$path] -cne $Before[$path]) { throw 'A rejected legacy journal changed the synthetic install tree.' }
    }
}

function Set-JournalNewFiles([string]$TransactionRoot, [object[]]$NewFiles) {
    $journalPath = Join-Path $TransactionRoot 'journal.json'
    $journal = [IO.File]::ReadAllText($journalPath) | ConvertFrom-Json
    $journal.new_files = @($NewFiles)
    [IO.File]::WriteAllText($journalPath, ($journal | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding($false)))
}

function Assert-PreflightRejectsWithoutMutation([string]$CaseName, [string]$InstallRoot, [string]$RecoveryPackageRoot, [string]$TransactionRoot) {
    $before = Get-FixtureSnapshot $InstallRoot
    $markerBefore = Get-Hash (Join-Path $InstallRoot 'user-marker.txt')
    $rejected = $false
    try {
        & (Join-Path $RecoveryPackageRoot 'Update-LocalAgentHarness.ps1') -PackageRoot $RecoveryPackageRoot -InstallRoot $InstallRoot | Out-Null
    } catch {
        if ($_.Exception.Message -notmatch 'Lifecycle recovery is required; no unverified files were changed') { throw }
        $rejected = $true
    }
    $after = Get-FixtureSnapshot $InstallRoot
    if (-not $rejected) { throw "The $CaseName invalid journal was not rejected." }
    Assert-SnapshotEqual -Before $before -After $after
    if ((Get-Hash (Join-Path $InstallRoot 'user-marker.txt')) -cne $markerBefore) { throw 'Unknown synthetic user data changed during preflight.' }
    if (-not (Test-Path -LiteralPath (Join-Path $TransactionRoot 'journal.json') -PathType Leaf)) { throw 'Preflight rejection removed the interrupted journal.' }
}

function New-InterruptedFixture([string]$Name, [string]$Version, [string]$CaseRoot) {
    $sourceRoot = if ($Version -ceq 'RC3') { $script:RC3PackageRoot } else { $script:RC4PackageRoot }
    $installRoot = Join-Path $CaseRoot 'install'
    $variantRoot = Join-Path $CaseRoot 'variant'
    $scriptsRoot = Join-Path $CaseRoot 'scripts'
    [IO.Directory]::CreateDirectory($installRoot) | Out-Null
    [IO.File]::WriteAllText((Join-Path $installRoot 'user-marker.txt'), 'synthetic user data retained')
    & (Join-Path $sourceRoot 'Install-LocalAgentHarness.ps1') -PackageRoot $sourceRoot -InstallRoot $installRoot | Out-Null
    Update-PackageVariant -SourceRoot $sourceRoot -Destination $variantRoot
    New-InstrumentedScripts -PackageRoot $sourceRoot -Destination $scriptsRoot
    Start-InterruptedUpdate -ScriptsRoot $scriptsRoot -PackageRoot $variantRoot -InstallRoot $installRoot -CaseRoot $CaseRoot
    return [pscustomobject]@{ InstallRoot=$installRoot; SourceRoot=$sourceRoot; TransactionRoot=(Get-TransactionRoot $installRoot); Name=$Name }
}

$rc3Names = @('local-agent-harness.exe','README.md','CLIENT_SETUP.md','Install-LocalAgentHarness.ps1','Update-LocalAgentHarness.ps1','Uninstall-LocalAgentHarness.ps1','LocalAgentHarness.Lifecycle.psm1','manifest.json','SHA256SUMS.txt')
$rc4Names = @('local-agent-harness.exe','README.md','CLIENT_SETUP.md','USER_GUIDE.ko.md','CLAUDE_DESKTOP.md','Install-LocalAgentHarness.ps1','Update-LocalAgentHarness.ps1','Uninstall-LocalAgentHarness.ps1','LocalAgentHarness.Lifecycle.psm1','manifest.json','SHA256SUMS.txt')
Assert-PackageBinding -PackageRoot $RC3PackageRoot -ArchivePath $RC3PackageArchivePath -ExpectedSha256 $ExpectedRC3ArchiveSha256 -ExpectedNames $rc3Names
Assert-PackageBinding -PackageRoot $RC4PackageRoot -ArchivePath $RC4PackageArchivePath -ExpectedSha256 $ExpectedRC4ArchiveSha256 -ExpectedNames $rc4Names

$sourceRoot = Split-Path -Parent $PSScriptRoot
foreach ($name in @('Install-LocalAgentHarness.ps1','Update-LocalAgentHarness.ps1','Uninstall-LocalAgentHarness.ps1','LocalAgentHarness.Lifecycle.psm1')) {
    if ((Get-Hash (Join-Path $sourceRoot $name)) -cne (Get-Hash (Join-Path $RC4PackageRoot $name))) {
        throw "Current lifecycle source does not match the pinned RC4 package member: $name"
    }
}

[IO.Directory]::CreateDirectory($OutputRoot) | Out-Null
$resolvedOutput = [IO.Path]::GetFullPath($OutputRoot).TrimEnd('\')
$runRoot = Join-Path $resolvedOutput ('legacy-journal-' + [Guid]::NewGuid().ToString('N'))
if (Test-Path -LiteralPath $runRoot) { throw 'The synthetic run root already exists.' }
[IO.Directory]::CreateDirectory($runRoot) | Out-Null
$script:RC3PackageRoot = [IO.Path]::GetFullPath($RC3PackageRoot)
$script:RC4PackageRoot = [IO.Path]::GetFullPath($RC4PackageRoot)
$checks = New-Object 'System.Collections.Generic.List[object]'

# Positive: an actual RC3 package creates the interrupted journal; RC4 recovery
# rolls it back, then the same invocation completes the requested RC4 update.
$caseRoot = Join-Path $runRoot 'positive-rc3-to-rc4'
[IO.Directory]::CreateDirectory($caseRoot) | Out-Null
$case = New-InterruptedFixture -Name 'positive' -Version 'RC3' -CaseRoot $caseRoot
$legacyJournal = [IO.File]::ReadAllText((Join-Path $case.TransactionRoot 'journal.json')) | ConvertFrom-Json
if (@($legacyJournal.new_files).Count -ne $rc3Names.Count -or @($legacyJournal.new_files | Where-Object { $rc3Names -cnotcontains [string]$_.path }).Count -ne 0) {
    throw 'The positive fixture did not produce the exact pinned RC3 install-name set.'
}
$markerHash = Get-Hash (Join-Path $case.InstallRoot 'user-marker.txt')
& (Join-Path $RC4PackageRoot 'Update-LocalAgentHarness.ps1') -PackageRoot $RC4PackageRoot -InstallRoot $case.InstallRoot | Out-Null
$receipt = [IO.File]::ReadAllText((Join-Path $case.InstallRoot '.lah-install-receipt.json')) | ConvertFrom-Json
if (@($receipt.files).Count -ne $rc4Names.Count -or @($receipt.files | Where-Object { $rc4Names -cnotcontains [string]$_.path }).Count -ne 0) {
    throw 'RC4 retry did not write the exact current install receipt.'
}
foreach ($entry in @($receipt.files)) {
    $path = Join-Path $case.InstallRoot ([string]$entry.path)
    if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or [long](Get-Item -LiteralPath $path -Force).Length -ne [long]$entry.size -or (Get-Hash $path) -cne [string]$entry.sha256) {
        throw 'An RC4 receipt member did not match its recorded size and hash.'
    }
}
if (@(Get-ChildItem -LiteralPath $case.InstallRoot -Force -Directory | Where-Object { $_.Name -like '.lah-transaction-*' }).Count -ne 0) {
    throw 'Successful RC4 recovery left a transaction directory behind.'
}
if ((Get-Hash (Join-Path $case.InstallRoot 'user-marker.txt')) -cne $markerHash) { throw 'The positive recovery changed synthetic user data.' }
$checks.Add([pscustomobject]@{case='interrupted-pinned-rc3-update-then-rc4-recovery-and-retry'; outcome='passed'; rc3_journal_files=$rc3Names.Count; rc4_receipt_files=$rc4Names.Count; marker_preserved=$true})

# Negative: the current transaction with one RC4 guide omitted is not accepted.
$caseRoot = Join-Path $runRoot 'negative-rc4-ten-of-eleven'
[IO.Directory]::CreateDirectory($caseRoot) | Out-Null
$case = New-InterruptedFixture -Name 'missing-current-member' -Version 'RC4' -CaseRoot $caseRoot
$journal = [IO.File]::ReadAllText((Join-Path $case.TransactionRoot 'journal.json')) | ConvertFrom-Json
$shortSet = @($journal.new_files | Where-Object { [string]$_.path -cne 'USER_GUIDE.ko.md' })
if ($shortSet.Count -ne 10) { throw 'The missing-member negative fixture did not produce a ten-name set.' }
Set-JournalNewFiles -TransactionRoot $case.TransactionRoot -NewFiles $shortSet
Assert-PreflightRejectsWithoutMutation -CaseName 'ten-of-eleven current payload' -InstallRoot $case.InstallRoot -RecoveryPackageRoot $RC4PackageRoot -TransactionRoot $case.TransactionRoot
$checks.Add([pscustomobject]@{case='reject-ten-of-eleven-current-set'; outcome='passed'; no_mutation=$true})

# Negative: nine names that mix one current-only document into the old set are
# rejected even though each individual path is recognized by RC4.
$caseRoot = Join-Path $runRoot 'negative-mixed-nine'
[IO.Directory]::CreateDirectory($caseRoot) | Out-Null
$case = New-InterruptedFixture -Name 'mixed-legacy-set' -Version 'RC3' -CaseRoot $caseRoot
$journal = [IO.File]::ReadAllText((Join-Path $case.TransactionRoot 'journal.json')) | ConvertFrom-Json
$mixedSet = @($journal.new_files)
$mixedEntry = @($mixedSet | Where-Object { [string]$_.path -ceq 'CLIENT_SETUP.md' })[0]
if ($null -eq $mixedEntry) { throw 'The mixed-set fixture could not find its expected legacy member.' }
$mixedEntry.path = 'CLAUDE_DESKTOP.md'
if ($mixedSet.Count -ne 9 -or @($mixedSet | Where-Object { $rc3Names -cnotcontains [string]$_.path }).Count -ne 1) {
    throw 'The mixed-set negative fixture is not the intended nine-name set.'
}
Set-JournalNewFiles -TransactionRoot $case.TransactionRoot -NewFiles $mixedSet
Assert-PreflightRejectsWithoutMutation -CaseName 'mixed nine-name set' -InstallRoot $case.InstallRoot -RecoveryPackageRoot $RC4PackageRoot -TransactionRoot $case.TransactionRoot
$checks.Add([pscustomobject]@{case='reject-mixed-nine-name-set'; outcome='passed'; no_mutation=$true})

$report = [ordered]@{
    schema='lah-lifecycle-legacy-journal-test-report-v1'
    runtime=$PSVersionTable.PSVersion.ToString()
    edition=$PSVersionTable.PSEdition
    rc3_archive_sha256=$ExpectedRC3ArchiveSha256.ToLowerInvariant()
    rc4_archive_sha256=$ExpectedRC4ArchiveSha256.ToLowerInvariant()
    checks=@($checks.ToArray())
}
[IO.File]::WriteAllText((Join-Path $runRoot 'report.json'), ($report | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
Write-Output ("Legacy journal compatibility passed: {0} checks under PowerShell {1}." -f $checks.Count, $PSVersionTable.PSVersion)
