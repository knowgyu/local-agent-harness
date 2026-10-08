[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$PackageZip,
    [Parameter(Mandatory)][string]$PackageReceipt,
    [Parameter(Mandatory)][string]$PackageRoot,
    [Parameter(Mandatory)][string]$LifecycleSource,
    [Parameter(Mandatory)][string]$OwnerHarness
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:ExpectedZipSha256 = '2c8c61e16826c912352fbdfd23ddd706a95e5d43fc68d1480316f704d702f569'
$script:ExpectedReceiptSha256 = 'a4b3ac2eea1fe9b55d1cfaa381af0e74418b9636547c6075658ce4163b2bda22'
$script:ExpectedModuleSha256 = '747da9cd567cedead211e9b1b36637c6f84ca9e64e03599242554ee36f3a8d11'
$script:ExpectedOwnerHarnessSha256 = '9fb431be330f757f8ba92cd35692b9033e700e2107297cf9c43811339377de26'
$script:FixedRecoveryError = 'Lifecycle recovery is required; no unverified files were changed.'

function Get-Sha256([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Assert-InputPin([string]$Path, [string]$Expected) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw 'A pinned acceptance input is missing.' }
    if ((Get-Sha256 $Path) -cne $Expected) { throw 'A pinned acceptance input hash did not match.' }
}

function Assert-PackageTree {
    $receipt = Get-Content -LiteralPath $script:PackageReceipt -Raw | ConvertFrom-Json
    if ([string]$receipt.package_zip_sha256 -cne $script:ExpectedZipSha256) { throw 'The package receipt does not bind the pinned ZIP.' }
    $expected = @($receipt.package_members | ForEach-Object { [string]$_.path } | Sort-Object)
    $files = @(Get-ChildItem -LiteralPath $script:PackageRoot -Force -File)
    $actual = @($files | ForEach-Object { [string]$_.Name } | Sort-Object)
    if ($expected.Count -ne 9 -or $actual.Count -ne $expected.Count -or ($expected -join "`n") -cne ($actual -join "`n")) {
        throw 'The extracted package member set did not match its receipt.'
    }
    foreach ($member in $receipt.package_members) {
        $path = Join-Path $script:PackageRoot ([string]$member.path)
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw 'An extracted package member is missing.' }
        $item = Get-Item -LiteralPath $path -Force
        if ($item.Length -ne [long]$member.size -or (Get-Sha256 $path) -cne [string]$member.sha256) {
            throw 'An extracted package member did not match its receipt.'
        }
    }
}

function Get-TreeInventory([string]$Root) {
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    $prefix = $fullRoot + '\'
    $inventory = @{}
    $inventory['.'] = 'directory'
    foreach ($item in @(Get-ChildItem -LiteralPath $fullRoot -Force -Recurse)) {
        $relative = $item.FullName.Substring($prefix.Length)
        if ($item.PSIsContainer) {
            $inventory[$relative] = 'directory'
        } else {
            $inventory[$relative] = 'file|' + [string]$item.Length + '|' + (Get-Sha256 $item.FullName)
        }
    }
    return $inventory
}

function Test-InventoryEqual([hashtable]$Before, [hashtable]$After) {
    if ($Before.Count -ne $After.Count) { return $false }
    foreach ($key in $Before.Keys) {
        if (-not $After.ContainsKey($key) -or [string]$Before[$key] -cne [string]$After[$key]) { return $false }
    }
    return $true
}

function New-OwnerFixtureHelperScriptBlock {
    $tokens = $null
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($script:OwnerHarness, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -ne 0) { throw 'The frozen owner fixture harness did not parse.' }
    $script:NeededOwnerFunctions = @(
        'Get-Hash',
        'Assert-WithinOwnerRoot',
        'New-TestPackage',
        'New-InstrumentedScripts',
        'Invoke-RegularAction',
        'Start-ChildAtBarrier',
        'Get-OwnedInventory',
        'New-Fixture',
        'Assert-Marker'
    )
    $found = @{}
    $functionAsts = @($ast.FindAll({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
            $script:NeededOwnerFunctions -ccontains [string]$node.Name
    }, $true))
    foreach ($functionAst in $functionAsts) { $found[[string]$functionAst.Name] = $functionAst }
    foreach ($name in $script:NeededOwnerFunctions) {
        if (-not $found.ContainsKey($name)) { throw 'A required frozen owner fixture helper is missing.' }
    }
    $definitions = @($script:NeededOwnerFunctions | ForEach-Object { $found[$_].Extent.Text })
    return [scriptblock]::Create([string]::Join("`r`n", $definitions))
}

function Get-TransactionDirectories([string]$InstallRoot) {
    return @(
        Get-ChildItem -LiteralPath $InstallRoot -Force -Directory |
            Where-Object { $_.Name -cmatch '^\.lah-transaction-[0-9a-f]{32}$' } |
            Sort-Object -Property Name
    )
}

function New-InterruptedUpdateFixture([string]$Name) {
    $case = New-Fixture -Name $Name -PackageVariant 'v1'
    $markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
    Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
    $installedInventory = Get-OwnedInventory $case.Install
    if ($installedInventory.Count -lt 2) { throw 'The synthetic baseline package did not install completely.' }

    $newPackage = Join-Path $case.Base 'package-root-v2'
    New-TestPackage -Destination $newPackage -Variant 'v2'
    New-InstrumentedScripts -Destination $case.Scripts -BarrierPoint 'update-first-backup'
    $child = Start-ChildAtBarrier -ScriptDirectory $case.Scripts -Action 'update' -Package $newPackage -InstallRoot $case.Install -Point 'update-first-backup'

    $transactions = @(Get-TransactionDirectories $case.Install)
    if ($transactions.Count -ne 1) { throw 'The interrupted update did not produce one owned transaction fixture.' }
    $journalPath = Join-Path $transactions[0].FullName 'journal.json'
    if (-not (Test-Path -LiteralPath $journalPath -PathType Leaf)) { throw 'The interrupted update fixture has no canonical journal.' }
    return [pscustomobject]@{
        Base = $case.Base
        Scripts = $case.Scripts
        Install = $case.Install
        Package = $case.Package
        NewPackage = $newPackage
        Transaction = $transactions[0].FullName
        Journal = $journalPath
        MarkerHash = $markerHash
        ChildWasStoppedAndWaited = $true
        ChildId = [int]$child.ProcessId
    }
}

function Assert-FixedErrorAndNoMutation([string]$CaseName, $Case) {
    $before = Get-TreeInventory $Case.Install
    $fixedErrorObserved = $false
    try {
        Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $Case.Package -InstallRoot $Case.Install | Out-Null
    } catch {
        $fixedErrorObserved = ([string]$_.Exception.Message -ceq $script:FixedRecoveryError)
    }
    $after = Get-TreeInventory $Case.Install
    $unchanged = Test-InventoryEqual -Before $before -After $after
    if (-not $fixedErrorObserved -or -not $unchanged) { throw 'A recovery preflight case failed its fixed-error/no-mutation acceptance.' }
    return [pscustomobject]@{
        case = $CaseName
        outcome = 'fixed-error-no-mutation'
        root_transaction_files_before_after_equal = $true
        inventory_entries = $before.Count
        child_interruption = 'owner-barrier-force-stop-wait'
    }
}

function Add-TestBytes([string]$Path, [string]$Text) {
    $bytes = [Text.Encoding]::ASCII.GetBytes($Text)
    $stream = [IO.File]::Open($Path, [IO.FileMode]::Append, [IO.FileAccess]::Write, [IO.FileShare]::Read)
    try { $stream.Write($bytes, 0, $bytes.Length); $stream.Flush() } finally { $stream.Dispose() }
}

function Invoke-CaseRootFileHashMismatch {
    $case = New-InterruptedUpdateFixture 'late-root-file-hash-mismatch'
    $journal = Get-Content -LiteralPath $case.Journal -Raw | ConvertFrom-Json
    $backupRoot = Join-Path $case.Transaction 'backup'
    $backedUp = @()
    if (Test-Path -LiteralPath $backupRoot -PathType Container) {
        $backedUp = @(Get-ChildItem -LiteralPath $backupRoot -Force -File | ForEach-Object { [string]$_.Name })
    }
    $candidate = $null
    $records = @($journal.old_files | Sort-Object -Property @{ Expression = { [string]$_.path }; Descending = $true })
    foreach ($record in $records) {
        $relative = [string]$record.path
        $name = [IO.Path]::GetFileName($relative)
        $path = Join-Path $case.Install $relative
        if ($backedUp -ccontains $name -or $name -match '\.exe$') { continue }
        if (Test-Path -LiteralPath $path -PathType Leaf) { $candidate = $path; break }
    }
    if ($null -eq $candidate -or $backedUp.Count -lt 1) { throw 'The update fixture lacks a later root-owned file suitable for mismatch injection.' }
    Add-TestBytes -Path $candidate -Text 'qa-synthetic-mismatch'
    return Assert-FixedErrorAndNoMutation -CaseName 'later-root-owned-file-hash-mismatch' -Case $case
}

function Invoke-CaseMultipleTransactions {
    $case = New-InterruptedUpdateFixture 'valid-plus-malformed-transaction'
    $secondRoot = Join-Path $case.Install ('.lah-transaction-' + [Guid]::NewGuid().ToString('N'))
    [IO.Directory]::CreateDirectory($secondRoot) | Out-Null
    [IO.File]::WriteAllText((Join-Path $secondRoot 'journal.json'), '{"schema":', (New-Object Text.UTF8Encoding($false)))
    return Assert-FixedErrorAndNoMutation -CaseName 'valid-transaction-plus-malformed-second-transaction' -Case $case
}

function Invoke-CaseOversizedJournal {
    $case = New-InterruptedUpdateFixture 'oversized-journal'
    $stream = [IO.File]::Open($case.Journal, [IO.FileMode]::Create, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try { $stream.SetLength(1048577); $stream.Flush() } finally { $stream.Dispose() }
    return Assert-FixedErrorAndNoMutation -CaseName 'journal-over-one-mibibyte' -Case $case
}

function Invoke-CaseUnknownSchemaField {
    $case = New-InterruptedUpdateFixture 'unknown-schema-field'
    $journal = Get-Content -LiteralPath $case.Journal -Raw | ConvertFrom-Json
    Add-Member -InputObject $journal -MemberType NoteProperty -Name 'qa_unknown_field' -Value 'synthetic-fixture-only' -Force
    $json = ConvertTo-Json -InputObject $journal -Depth 20
    [IO.File]::WriteAllText($case.Journal, $json, (New-Object Text.UTF8Encoding($false)))
    return Assert-FixedErrorAndNoMutation -CaseName 'unknown-journal-schema-field' -Case $case
}

$script:PackageZip = [IO.Path]::GetFullPath($PackageZip)
$script:PackageReceipt = [IO.Path]::GetFullPath($PackageReceipt)
$script:PackageRoot = [IO.Path]::GetFullPath($PackageRoot)
$script:LifecycleSource = [IO.Path]::GetFullPath($LifecycleSource)
$script:OwnerHarness = [IO.Path]::GetFullPath($OwnerHarness)
$script:OutputDirectory = [IO.Path]::GetFullPath($PSScriptRoot)
$script:ExpectedOutputLeaf = 'hardening-recovery-preflight-qa-20261006-r1'
if ((Split-Path -Leaf $script:OutputDirectory) -cne $script:ExpectedOutputLeaf) { throw 'The QA harness is outside its dedicated output directory.' }

Assert-InputPin -Path $script:PackageZip -Expected $script:ExpectedZipSha256
Assert-InputPin -Path $script:PackageReceipt -Expected $script:ExpectedReceiptSha256
Assert-InputPin -Path (Join-Path $script:PackageRoot 'LocalAgentHarness.Lifecycle.psm1') -Expected $script:ExpectedModuleSha256
Assert-InputPin -Path (Join-Path $script:LifecycleSource 'LocalAgentHarness.Lifecycle.psm1') -Expected $script:ExpectedModuleSha256
Assert-InputPin -Path $script:OwnerHarness -Expected $script:ExpectedOwnerHarnessSha256
Assert-PackageTree

$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')
$script:RunRoot = [IO.Path]::GetFullPath((Join-Path $tempRoot ('LAH-RecoveryPreflight-qa-20261006-' + $PSVersionTable.PSVersion.ToString() + '-' + [Guid]::NewGuid().ToString('N'))))
$script:LifecycleSource = [IO.Path]::GetFullPath($script:LifecycleSource)
$script:PackageRoot = [IO.Path]::GetFullPath($script:PackageRoot)
$ownerHelpers = New-OwnerFixtureHelperScriptBlock
. $ownerHelpers
Assert-WithinOwnerRoot -Path $script:RunRoot -OwnerRoot $tempRoot | Out-Null
[IO.Directory]::CreateDirectory($script:RunRoot) | Out-Null

$results = New-Object 'System.Collections.Generic.List[object]'
$results.Add((Invoke-CaseRootFileHashMismatch))
$results.Add((Invoke-CaseMultipleTransactions))
$results.Add((Invoke-CaseOversizedJournal))
$results.Add((Invoke-CaseUnknownSchemaField))

$report = [ordered]@{
    schema = 'lah-recovery-preflight-acceptance-report-v1'
    runtime = $PSVersionTable.PSVersion.ToString()
    edition = $PSVersionTable.PSEdition
    os = [Environment]::OSVersion.VersionString
    candidate_zip_sha256 = $script:ExpectedZipSha256
    candidate_receipt_sha256 = $script:ExpectedReceiptSha256
    lifecycle_module_sha256 = $script:ExpectedModuleSha256
    owner_fixture_harness_sha256 = $script:ExpectedOwnerHarnessSha256
    interrupted_fixture = 'owner update-first-backup deterministic barrier; owned child force-stopped and waited'
    case_count = $results.Count
    all_fixed_error_and_unchanged = $true
    fixtures_preserved = $true
    fixture_root_kind = 'fresh owned child of the Windows TEMP directory'
    no_per_file_hashes_or_contents_recorded = $true
    cases = @($results.ToArray())
}
$runtimeTag = if ($PSVersionTable.PSEdition -ceq 'Desktop') { 'powershell-51' } else { 'pwsh-7' }
$reportPath = Join-Path $script:OutputDirectory ('report-' + $runtimeTag + '.json')
[IO.File]::WriteAllText($reportPath, ($report | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
Write-Output ('Recovery preflight acceptance passed: {0} cases on {1} ({2}).' -f $results.Count, $report.runtime, $runtimeTag)
