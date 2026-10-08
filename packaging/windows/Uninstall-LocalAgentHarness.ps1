[CmdletBinding()]
param([string]$InstallRoot)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'LocalAgentHarness.Lifecycle.psm1') -Force
if ([string]::IsNullOrWhiteSpace($InstallRoot)) {
    if (Test-Path -LiteralPath (Join-Path $PSScriptRoot '.lah-install-receipt.json') -PathType Leaf) {
        $InstallRoot = $PSScriptRoot
    } else {
        $InstallRoot = Get-LAHDefaultInstallRoot
    }
}
Invoke-LAHUninstall -InstallRoot $InstallRoot
