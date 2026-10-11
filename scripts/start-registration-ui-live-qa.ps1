[CmdletBinding()]
param(
    [string]$ReuseSettingsPath,

    [ValidateRange(10, 300)]
    [int]$StartupTimeoutSeconds = 60
)

$ErrorActionPreference = 'Stop'

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$runtimeRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot 'output/ux-preview/runtime'))
[IO.Directory]::CreateDirectory($runtimeRoot) | Out-Null
$runtimeItem = Get-Item -LiteralPath $runtimeRoot
if (($runtimeItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw 'The live QA runtime directory must not be a reparse point.'
}

$baseName = 'registration-ui-live-qa-' + [guid]::NewGuid().ToString('N')
$baseDir = [IO.Path]::GetFullPath((Join-Path $runtimeRoot $baseName))
$runtimePrefix = $runtimeRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
if (-not $baseDir.StartsWith($runtimePrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The live QA output directory escaped output/ux-preview/runtime.'
}
[IO.Directory]::CreateDirectory($baseDir) | Out-Null
$baseItem = Get-Item -LiteralPath $baseDir
if (($baseItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw 'The live QA output directory must not be a reparse point.'
}

$stdoutPath = Join-Path $baseDir 'launcher.stdout.log'
$stderrPath = Join-Path $baseDir 'launcher.stderr.log'
$oldOutputDir = $env:LAH_REGISTRATION_UI_QA_DIR
$oldReusePath = $env:LAH_REGISTRATION_UI_QA_REUSE_SETTINGS
$go = Get-Command go -ErrorAction Stop

try {
    $env:LAH_REGISTRATION_UI_QA_DIR = $baseDir
    if ([string]::IsNullOrWhiteSpace($ReuseSettingsPath)) {
        Remove-Item Env:LAH_REGISTRATION_UI_QA_REUSE_SETTINGS -ErrorAction SilentlyContinue
    } else {
        if (-not [IO.Path]::IsPathRooted($ReuseSettingsPath)) {
            $ReuseSettingsPath = Join-Path $repoRoot $ReuseSettingsPath
        }
        $env:LAH_REGISTRATION_UI_QA_REUSE_SETTINGS = [IO.Path]::GetFullPath($ReuseSettingsPath)
    }

    # The helper keeps its own 12-hour timer. Go's default test timeout is 10 minutes,
    # so give the test runner one additional hour to allow the bounded review window.
    $launcher = Start-Process -FilePath $go.Source `
        -ArgumentList @('test', '-timeout=13h', '-run=^TestRegistrationUILiveQAHelper$', '-count=1') `
        -WorkingDirectory $repoRoot -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath

    $deadline = [DateTime]::UtcNow.AddSeconds($StartupTimeoutSeconds)
    $metadataPath = $null
    while ([DateTime]::UtcNow -lt $deadline) {
        $metadata = Get-ChildItem -LiteralPath $baseDir -Filter metadata.json -Recurse -File -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($null -ne $metadata) {
            $metadataPath = $metadata.FullName
            break
        }
        $launcher.Refresh()
        if ($launcher.HasExited) {
            break
        }
        Start-Sleep -Milliseconds 350
    }

    if ($null -eq $metadataPath) {
        if (Test-Path -LiteralPath $stdoutPath) { Get-Content -LiteralPath $stdoutPath -Tail 40 }
        if (Test-Path -LiteralPath $stderrPath) { Get-Content -LiteralPath $stderrPath -Tail 40 }
        throw "The QA helper did not publish metadata. Launcher PID=$($launcher.Id); ExitCode=$($launcher.ExitCode). No process was terminated."
    }

    $metadata = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json
    $helper = Get-Process -Id ([int]$metadata.pid) -ErrorAction Stop
    [pscustomobject]@{
        URL = $metadata.url
        Metadata = $metadataPath
        PID = $metadata.pid
        StopFile = $metadata.stop_file
        ConfigPath = $metadata.config_path
        SettingsMode = $metadata.settings_mode
        SetupDraftProposalQueueMode = $metadata.setup_draft_proposal_queue_mode
        LauncherPID = $launcher.Id
        HelperProcess = $helper.ProcessName
    }
} finally {
    if ($null -eq $oldOutputDir) {
        Remove-Item Env:LAH_REGISTRATION_UI_QA_DIR -ErrorAction SilentlyContinue
    } else {
        $env:LAH_REGISTRATION_UI_QA_DIR = $oldOutputDir
    }
    if ($null -eq $oldReusePath) {
        Remove-Item Env:LAH_REGISTRATION_UI_QA_REUSE_SETTINGS -ErrorAction SilentlyContinue
    } else {
        $env:LAH_REGISTRATION_UI_QA_REUSE_SETTINGS = $oldReusePath
    }
}
