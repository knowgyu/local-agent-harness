[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$PackageRoot,
    [Parameter(Mandatory)][string]$OutputRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-FileSHA256([string]$Path) {
    (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-FlatDirectoryInventory([string]$Path) {
    $lines = New-Object 'System.Collections.Generic.List[string]'
    foreach ($item in @(Get-ChildItem -LiteralPath $Path -Force | Sort-Object Name)) {
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            [void]$lines.Add(('REPARSE|{0}|{1}' -f $item.Name, $item.LinkType))
        } elseif ($item.PSIsContainer) {
            [void]$lines.Add(('DIR|{0}' -f $item.Name))
        } else {
            [void]$lines.Add(('FILE|{0}|{1}|{2}' -f $item.Name, $item.Length, (Get-FileSHA256 $item.FullName)))
        }
    }
    return $lines.ToArray()
}

$package = [IO.Path]::GetFullPath($PackageRoot)
$output = [IO.Path]::GetFullPath($OutputRoot)
if (-not (Test-Path -LiteralPath $package -PathType Container)) { throw 'PackageRoot is missing.' }
if (Test-Path -LiteralPath $output) { throw 'OutputRoot must be a new test-owned directory.' }
[IO.Directory]::CreateDirectory($output) | Out-Null

$allowedNames = @(
    'local-agent-harness.exe',
    'README.md',
    'CLIENT_SETUP.md',
    'Install-LocalAgentHarness.ps1',
    'Update-LocalAgentHarness.ps1',
    'Uninstall-LocalAgentHarness.ps1',
    'LocalAgentHarness.Lifecycle.psm1',
    'manifest.json',
    'SHA256SUMS.txt'
)
$members = @(Get-ChildItem -LiteralPath $package -Force -File)
if ($members.Count -ne 9 -or @(Get-ChildItem -LiteralPath $package -Force -Directory).Count -ne 0) {
    throw 'PackageRoot must be the exact flat nine-file package.'
}
foreach ($member in $members) {
    if ($allowedNames -cnotcontains $member.Name) { throw 'PackageRoot contains an unexpected member.' }
}
foreach ($name in $allowedNames) {
    if (-not (Test-Path -LiteralPath (Join-Path $package $name) -PathType Leaf)) { throw "PackageRoot is missing $name." }
}

$fixture = Join-Path $output ('fixture-' + [Guid]::NewGuid().ToString('N'))
$target = Join-Path $fixture 'physical-target'
$link = Join-Path $fixture 'linked-parent'
$installRoot = Join-Path $link 'new-install'
[IO.Directory]::CreateDirectory($target) | Out-Null
$marker = Join-Path $target 'user-owned-marker.txt'
[IO.File]::WriteAllText($marker, 'preserve this test-owned marker', (New-Object Text.UTF8Encoding($false)))
$markerBefore = Get-FileSHA256 $marker
$beforeInventory = @(Get-FlatDirectoryInventory $target)

$linkType = $null
$linkFailure = $null
try {
    New-Item -ItemType SymbolicLink -Path $link -Target $target -ErrorAction Stop | Out-Null
    $linkType = 'SymbolicLink'
} catch {
    if (Test-Path -LiteralPath $link) { throw 'Symbolic-link creation left an unexpected object; fixture preserved.' }
    $linkFailure = $_.Exception.GetType().Name
    try {
        New-Item -ItemType Junction -Path $link -Target $target -ErrorAction Stop | Out-Null
        $linkType = 'Junction'
    } catch {
        if (Test-Path -LiteralPath $link) { throw 'Junction creation left an unexpected object; fixture preserved.' }
        throw 'This current user could not create a symbolic link or NTFS junction; no privilege change was attempted.'
    }
}
$linkItem = Get-Item -LiteralPath $link -Force
if (($linkItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) { throw 'The parent redirect is not a reparse point.' }
$resolvedTarget = [IO.Path]::GetFullPath([string]$linkItem.Target).TrimEnd('\')
if (-not [string]::Equals($resolvedTarget, $target.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The parent redirect does not point to the physical target fixture.'
}

$installScript = Join-Path $package 'Install-LocalAgentHarness.ps1'
$installError = $null
try {
    & $installScript -PackageRoot $package -InstallRoot $installRoot | Out-Null
} catch {
    $installError = $_.Exception.Message
}

$afterInventory = @(Get-FlatDirectoryInventory $target)
$newRootCreated = Test-Path -LiteralPath (Join-Path $target 'new-install') -PathType Container
$markerAfter = Get-FileSHA256 $marker
$expectedFixedError = $installError -match 'Reparse points are not allowed in managed paths'
$inventoryUnchanged = [string]::Equals(
    [string]::Join([Environment]::NewLine, $beforeInventory),
    [string]::Join([Environment]::NewLine, $afterInventory),
    [StringComparison]::Ordinal
)
$status = 'passed'
$failure = $null
if (-not $expectedFixedError) {
    $status = 'failed'
    $failure = 'The install did not return the fixed reparse-path rejection.'
} elseif ($newRootCreated) {
    $status = 'failed'
    $failure = 'The install created the missing root through its reparse-point ancestor.'
} elseif (-not $inventoryUnchanged) {
    $status = 'failed'
    $failure = 'The physical target inventory changed.'
} elseif ($markerBefore -cne $markerAfter) {
    $status = 'failed'
    $failure = 'The unowned marker changed.'
}

$report = [ordered]@{
    schema = 'lah-lifecycle-reparse-root-creation-test-v1'
    status = $status
    runtime = [ordered]@{
        powershell_version = $PSVersionTable.PSVersion.ToString()
        edition = [string]$PSVersionTable.PSEdition
        windows_version = [Environment]::OSVersion.Version.ToString()
        architecture = $env:PROCESSOR_ARCHITECTURE
    }
    package_root = $package
    test_script_path = [IO.Path]::GetFullPath($PSCommandPath)
    test_script_sha256 = Get-FileSHA256 $PSCommandPath
    module_sha256 = Get-FileSHA256 (Join-Path $package 'LocalAgentHarness.Lifecycle.psm1')
    fixture_root = $fixture
    redirect_type = $linkType
    symbolic_link_failure = $linkFailure
    install_root = $installRoot
    fixed_rejection_observed = $expectedFixedError
    new_root_created_through_redirect = $newRootCreated
    marker_preserved = ($markerBefore -ceq $markerAfter)
    inventory_unchanged = $inventoryUnchanged
    before_inventory = @($beforeInventory)
    after_inventory = @($afterInventory)
    product_executable_launched = $false
    failure = $failure
}
$reportPath = Join-Path $output 'report.json'
[IO.File]::WriteAllText($reportPath, ($report | ConvertTo-Json -Depth 6) + [Environment]::NewLine, (New-Object Text.UTF8Encoding($false)))
if ($status -ne 'passed') { throw "Lifecycle reparse root-creation test failed; fixture and report were retained at $reportPath. $failure" }
Write-Output ("PASS lifecycle reparse root-creation test under PowerShell {0}; report: {1}" -f $PSVersionTable.PSVersion, $reportPath)
