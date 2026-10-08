[CmdletBinding()]
param([Parameter(Mandatory)][string]$LifecycleModule, [Parameter(Mandatory)][string]$OutputRoot)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$modulePath = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $LifecycleModule).Path)
[IO.Directory]::CreateDirectory($OutputRoot) | Out-Null
$ownerRoot = [IO.Path]::GetFullPath((Join-Path $OutputRoot ('lease-' + $PSVersionTable.PSVersion.ToString() + '-' + [Guid]::NewGuid().ToString('N'))))
$parent = Join-Path $ownerRoot 'parent'
$installRoot = Join-Path $parent 'install-root'
$heldRoot = Join-Path $parent 'held-root'
$heldParent = Join-Path $ownerRoot 'held-parent'
[IO.Directory]::CreateDirectory($installRoot) | Out-Null
$marker = Join-Path $installRoot '.owner-marker'
[IO.File]::WriteAllText($marker, 'owned lease test')
$markerHash = (Get-FileHash -LiteralPath $marker -Algorithm SHA256).Hash.ToLowerInvariant()
$module = Import-Module -Name $modulePath -Force -PassThru
$mutex = $null
$lease = $null
try {
    $mutex = & $module { param($path) Enter-LAHOperationMutex -InstallRoot $path } $installRoot
    $lease = & $module { param($path) Open-LAHRootLease -InstallRoot $path } $installRoot
    $rootRenameBlocked = $false
    try { [IO.Directory]::Move($installRoot, $heldRoot) } catch { $rootRenameBlocked = $true }
    $parentRenameBlocked = $false
    try { [IO.Directory]::Move($parent, $heldParent) } catch { $parentRenameBlocked = $true }
    $markerPath = Join-Path $installRoot '.owner-marker'
    $markerPreserved = (Test-Path -LiteralPath $markerPath -PathType Leaf) -and (Get-FileHash -LiteralPath $markerPath -Algorithm SHA256).Hash.ToLowerInvariant() -ceq $markerHash
    if (-not $rootRenameBlocked -or -not $parentRenameBlocked -or -not $markerPreserved) {
        throw 'The native directory lease did not block the tested root and immediate-parent rename operations.'
    }
} finally {
    if ($null -ne $lease) { $lease.Dispose() }
    if ($null -ne $mutex) { $mutex.ReleaseMutex(); $mutex.Dispose() }
    Remove-Module -ModuleInfo $module -ErrorAction SilentlyContinue
}
$report = [ordered]@{
    schema='lah-root-directory-lease-report-v1'
    runtime=$PSVersionTable.PSVersion.ToString()
    edition=$PSVersionTable.PSEdition
    module_sha256=(Get-FileHash -LiteralPath $modulePath -Algorithm SHA256).Hash.ToLowerInvariant()
    owner_root=$ownerRoot
    native_open_contract='CreateFileW(FILE_LIST_DIRECTORY, FILE_SHARE_READ|FILE_SHARE_WRITE, OPEN_EXISTING, BACKUP_SEMANTICS|OPEN_REPARSE_POINT)'
    root_rename_blocked=$rootRenameBlocked
    immediate_parent_rename_blocked=$parentRenameBlocked
    marker_preserved=$markerPreserved
    cleanup='fixture preserved for review'
}
$reportPath = Join-Path $ownerRoot 'report.json'
[IO.File]::WriteAllText($reportPath, ($report | ConvertTo-Json -Depth 5), (New-Object Text.UTF8Encoding($false)))
Write-Output ("Root lease acceptance passed under PowerShell {0}. Report: {1}" -f $report.runtime, $reportPath)
