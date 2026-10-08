[CmdletBinding()]
param(
    [string]$PackageRoot = $PSScriptRoot,
    [string]$InstallRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'LocalAgentHarness.Lifecycle.psm1') -Force
if ([string]::IsNullOrWhiteSpace($InstallRoot)) { $InstallRoot = Get-LAHDefaultInstallRoot }
Invoke-LAHApplyPackage -PackageRoot $PackageRoot -InstallRoot $InstallRoot -Operation 'install'
