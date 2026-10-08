[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BasePackageRoot,
    [Parameter(Mandatory)][string]$LifecycleModule,
    [Parameter(Mandatory)][string]$OutputDirectory,
    [Parameter(Mandatory)][string]$BasePackageZipSha256
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$baseRoot = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $BasePackageRoot).Path)
$modulePath = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $LifecycleModule).Path)
$out = [IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($out) | Out-Null
$candidateName = 'local-agent-harness-portable-windows-amd64-lifecycle-recovery-test-20261006-r3'
$root = Join-Path $out 'package-root'
$archive = Join-Path $out ($candidateName + '.zip')
$receiptPath = Join-Path $out ($candidateName + '.receipt.json')
[IO.Directory]::CreateDirectory($root) | Out-Null
foreach ($file in @(Get-ChildItem -LiteralPath $baseRoot -Force -File)) {
    [IO.File]::Copy($file.FullName, (Join-Path $root $file.Name), $false)
}
[IO.File]::Copy($modulePath, (Join-Path $root 'LocalAgentHarness.Lifecycle.psm1'), $true)
$manifestPath = Join-Path $root 'manifest.json'
$manifest = [IO.File]::ReadAllText($manifestPath) | ConvertFrom-Json
foreach ($entry in @($manifest.files)) {
    $memberPath = Join-Path $root ([string]$entry.path)
    $entry.size = [long](Get-Item -LiteralPath $memberPath -Force).Length
    $entry.sha256 = (Get-FileHash -LiteralPath $memberPath -Algorithm SHA256).Hash.ToLowerInvariant()
}
[IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding($false)))
$sumLines = @(
    Get-ChildItem -LiteralPath $root -Force -File |
        Where-Object { $_.Name -cne 'SHA256SUMS.txt' } |
        Sort-Object Name |
        ForEach-Object { "$((Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant())  $($_.Name)" }
)
[IO.File]::WriteAllLines((Join-Path $root 'SHA256SUMS.txt'), $sumLines, (New-Object Text.UTF8Encoding($false)))
Add-Type -AssemblyName System.IO.Compression.FileSystem
[IO.Compression.ZipFile]::CreateFromDirectory($root, $archive, [IO.Compression.CompressionLevel]::Optimal, $false)
$zip = [IO.Compression.ZipFile]::OpenRead($archive)
try {
    $members = @($zip.Entries | Sort-Object FullName | ForEach-Object {
        if ($_.FullName.Contains('/') -or $_.FullName.Contains('\')) { throw 'Candidate package contains a nested ZIP member.' }
        $path = Join-Path $root $_.FullName
        [pscustomobject]@{
            path=$_.FullName
            size=[long](Get-Item -LiteralPath $path -Force).Length
            sha256=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        }
    })
} finally { $zip.Dispose() }
if ($members.Count -ne 9) { throw 'Candidate package does not contain the expected nine flat members.' }
$zipHash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
$receipt = [ordered]@{
    schema='lah-lifecycle-path-race-validation-receipt-v1'
    package_zip=[IO.Path]::GetFileName($archive)
    package_zip_sha256=$zipHash
    base_package_zip_sha256=$BasePackageZipSha256
    lifecycle_module_sha256=(Get-FileHash -LiteralPath $modulePath -Algorithm SHA256).Hash.ToLowerInvariant()
    member_count=$members.Count
    package_members=$members
    validation_scope='test-only candidate for exact-package Windows path-race review; not a release package'
}
[IO.File]::WriteAllText($receiptPath, ($receipt | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
$receiptHash = (Get-FileHash -LiteralPath $receiptPath -Algorithm SHA256).Hash.ToLowerInvariant()
Write-Output ([pscustomobject]@{ archive=$archive; archive_sha256=$zipHash; receipt=$receiptPath; receipt_sha256=$receiptHash; root_members=$members.Count; module_sha256=$receipt.lifecycle_module_sha256 } | ConvertTo-Json -Compress)
