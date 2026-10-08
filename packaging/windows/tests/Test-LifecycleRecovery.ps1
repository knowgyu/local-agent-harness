[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$PackageRoot,
    [Parameter(Mandatory)][string]$LifecycleSource,
    [Parameter(Mandatory)][string]$OutputRoot,
    [Parameter(Mandatory)][string]$PackageArchivePath,
    [Parameter(Mandatory)][string]$ExpectedPackageArchiveSha256
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-Hash([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Assert-PackageArchiveBinding([string]$ArchivePath, [string]$ExpectedSha256, [string]$ExtractedRoot, [string]$SourceRoot) {
    if ($ExpectedSha256 -cnotmatch '^[0-9a-fA-F]{64}$') { throw 'The expected package archive hash is malformed.' }
    if (-not (Test-Path -LiteralPath $ArchivePath -PathType Leaf)) { throw 'The exact package archive is missing.' }
    $archiveHash = Get-Hash $ArchivePath
    if ($archiveHash -cne $ExpectedSha256.ToLowerInvariant()) { throw 'The exact package archive hash does not match the supplied pin.' }

    $allowedNames = @('local-agent-harness.exe','README.md','CLIENT_SETUP.md','Install-LocalAgentHarness.ps1','Update-LocalAgentHarness.ps1','Uninstall-LocalAgentHarness.ps1','LocalAgentHarness.Lifecycle.psm1','manifest.json','SHA256SUMS.txt')
    $rootFiles = @(Get-ChildItem -LiteralPath $ExtractedRoot -Force -File)
    if ($rootFiles.Count -ne $allowedNames.Count -or @(Get-ChildItem -LiteralPath $ExtractedRoot -Force -Directory).Count -ne 0) {
        throw 'The extracted package root is not the exact flat nine-member package.'
    }
    foreach ($file in $rootFiles) { if ($allowedNames -cnotcontains $file.Name) { throw 'The extracted package root contains an unexpected member.' } }

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archive = [IO.Compression.ZipFile]::OpenRead($ArchivePath)
    $archiveMap = @{}
    try {
        $entries = @($archive.Entries)
        if ($entries.Count -ne $allowedNames.Count) { throw 'The pinned package archive does not contain nine members.' }
        foreach ($entry in $entries) {
            $name = [string]$entry.FullName
            if ($allowedNames -cnotcontains $name -or $name.Contains('/') -or $name.Contains([char]92) -or $archiveMap.ContainsKey($name)) {
                throw 'The pinned package archive has a nested, unexpected, or repeated member.'
            }
            $entryStream = $entry.Open()
            $sha = [Security.Cryptography.SHA256]::Create()
            try { $memberHash = [BitConverter]::ToString($sha.ComputeHash($entryStream)).Replace('-', '').ToLowerInvariant() }
            finally { $entryStream.Dispose(); $sha.Dispose() }
            $archiveMap[$name] = [pscustomobject]@{ size=[long]$entry.Length; sha256=$memberHash }
        }
    } finally { $archive.Dispose() }

    foreach ($name in $allowedNames) {
        $path = Join-Path $ExtractedRoot $name
        $item = Get-Item -LiteralPath $path -Force
        if (-not $archiveMap.ContainsKey($name) -or [long]$item.Length -ne [long]$archiveMap[$name].size -or (Get-Hash $path) -cne $archiveMap[$name].sha256) {
            throw "The extracted package member does not match the pinned ZIP: $name"
        }
    }
    foreach ($name in @('Install-LocalAgentHarness.ps1','Update-LocalAgentHarness.ps1','Uninstall-LocalAgentHarness.ps1','LocalAgentHarness.Lifecycle.psm1')) {
        $packagePath = Join-Path $ExtractedRoot $name
        $sourcePath = Join-Path $SourceRoot $name
        if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf) -or
            (Get-Item -LiteralPath $packagePath -Force).Length -ne (Get-Item -LiteralPath $sourcePath -Force).Length -or
            (Get-Hash $packagePath) -cne (Get-Hash $sourcePath)) {
            throw "Lifecycle source does not match the extracted final package member: $name"
        }
    }
    return $archiveHash
}

function Assert-WithinOwnerRoot([string]$Path, [string]$OwnerRoot) {
    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $fullOwner = [IO.Path]::GetFullPath($OwnerRoot).TrimEnd('\')
    if (-not $fullPath.StartsWith($fullOwner + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'A lifecycle test path escaped its owner fixture.'
    }
    return $fullPath
}

function New-TestPackage([string]$Destination, [string]$Variant) {
    [IO.Directory]::CreateDirectory($Destination) | Out-Null
    foreach ($item in @(Get-ChildItem -LiteralPath $script:PackageRoot -Force -File)) {
        [IO.File]::Copy($item.FullName, (Join-Path $Destination $item.Name), $false)
    }
    [IO.File]::Copy((Join-Path $script:LifecycleSource 'LocalAgentHarness.Lifecycle.psm1'), (Join-Path $Destination 'LocalAgentHarness.Lifecycle.psm1'), $true)
    if ($Variant -ceq 'v2') {
        $readme = Join-Path $Destination 'README.md'
        [IO.File]::AppendAllText($readme, "`nRecovery fixture package variant 2.`n", (New-Object Text.UTF8Encoding($false)))
    } elseif ($Variant -ceq 'size-one') {
        $readme = Join-Path $Destination 'README.md'
        [IO.File]::WriteAllText($readme, 'x', (New-Object Text.UTF8Encoding($false)))
    }
    $manifestPath = Join-Path $Destination 'manifest.json'
    $manifest = [IO.File]::ReadAllText($manifestPath) | ConvertFrom-Json
    foreach ($entry in @($manifest.files)) {
        $path = Join-Path $Destination ([string]$entry.path)
        $entry.size = [long](Get-Item -LiteralPath $path -Force).Length
        $entry.sha256 = Get-Hash $path
    }
    [IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding($false)))
    $lines = @(
        Get-ChildItem -LiteralPath $Destination -Force -File |
            Where-Object { $_.Name -cne 'SHA256SUMS.txt' } |
            Sort-Object Name |
            ForEach-Object { "$(Get-Hash $_.FullName)  $($_.Name)" }
    )
    [IO.File]::WriteAllLines((Join-Path $Destination 'SHA256SUMS.txt'), $lines, (New-Object Text.UTF8Encoding($false)))
}

function New-InstrumentedScripts([string]$Destination, [string]$BarrierPoint) {
    [IO.Directory]::CreateDirectory($Destination) | Out-Null
    foreach ($name in @('Install-LocalAgentHarness.ps1','Update-LocalAgentHarness.ps1','Uninstall-LocalAgentHarness.ps1')) {
        [IO.File]::Copy((Join-Path $script:LifecycleSource $name), (Join-Path $Destination $name), $false)
    }
    $modulePath = Join-Path $script:LifecycleSource 'LocalAgentHarness.Lifecycle.psm1'
    $moduleText = [IO.File]::ReadAllText($modulePath)
    $hook = @'
function Invoke-LAHTestBarrier {
    param([Parameter(Mandatory)][string]$Point)
    $hookRoot = Join-Path $PSScriptRoot '.test-hooks'
    [IO.Directory]::CreateDirectory($hookRoot) | Out-Null
    [IO.File]::WriteAllText((Join-Path $hookRoot ($Point + '.ready')), 'ready')
    while (-not (Test-Path -LiteralPath (Join-Path $hookRoot ($Point + '.continue')))) { Start-Sleep -Milliseconds 25 }
}
'@
    if ($BarrierPoint -ceq 'install-first-payload') {
        $anchor = '        foreach ($name in ($script:LAHInstallNames | Sort-Object)) { [IO.File]::Move((Join-Path $newRoot $name), (Join-Path $root $name)) }'
        $replacement = @'
        $script:LAHTestBarrierUsed = $false
        foreach ($name in ($script:LAHInstallNames | Sort-Object)) {
            [IO.File]::Move((Join-Path $newRoot $name), (Join-Path $root $name))
            if (-not $script:LAHTestBarrierUsed) { $script:LAHTestBarrierUsed = $true; Invoke-LAHTestBarrier -Point 'install-first-payload' }
        }
'@
    } elseif ($BarrierPoint -ceq 'update-first-backup') {
        $anchor = '        foreach ($name in $oldPresent) { [IO.File]::Move((Join-Path $root $name), (Join-Path $backupRoot $name)) }'
        $replacement = @'
        $script:LAHTestBarrierUsed = $false
        foreach ($name in $oldPresent) {
            [IO.File]::Move((Join-Path $root $name), (Join-Path $backupRoot $name))
            if (-not $script:LAHTestBarrierUsed) { $script:LAHTestBarrierUsed = $true; Invoke-LAHTestBarrier -Point 'update-first-backup' }
        }
'@
    } elseif ($BarrierPoint -ceq 'uninstall-before-receipt') {
        $anchor = '        foreach ($name in $oldPresent) { [IO.File]::Move((Join-Path $root $name), (Join-Path $transactionRoot $name)) }'
        $replacement = @'
        $script:LAHTestBarrierUsed = $false
        foreach ($name in $oldPresent) {
            [IO.File]::Move((Join-Path $root $name), (Join-Path $transactionRoot $name))
            if (-not $script:LAHTestBarrierUsed) { $script:LAHTestBarrierUsed = $true; Invoke-LAHTestBarrier -Point 'uninstall-before-receipt' }
        }
'@
    } elseif ($BarrierPoint -ceq 'uninstall-after-receipt') {
        $anchor = '        [IO.File]::Move($receipt.Path, (Join-Path $transactionRoot $script:LAHReceiptName))'
        $replacement = @'
        [IO.File]::Move($receipt.Path, (Join-Path $transactionRoot $script:LAHReceiptName))
        Invoke-LAHTestBarrier -Point 'uninstall-after-receipt'
'@
    } else {
        throw 'Unknown deterministic lifecycle test barrier.'
    }
    if (-not $moduleText.Contains($anchor)) { throw 'The test hook anchor did not match the pinned lifecycle source.' }
    $moduleText = $moduleText.Replace($anchor, $replacement)
    if ($BarrierPoint -cne 'uninstall-after-receipt') { $moduleText = $moduleText.Replace('Set-StrictMode -Version Latest', "Set-StrictMode -Version Latest`r`n$hook") }
    else { $moduleText = $moduleText.Replace('Set-StrictMode -Version Latest', "Set-StrictMode -Version Latest`r`n$hook") }
    [IO.File]::WriteAllText((Join-Path $Destination 'LocalAgentHarness.Lifecycle.psm1'), $moduleText, (New-Object Text.UTF8Encoding($false)))
    [IO.Directory]::CreateDirectory((Join-Path $Destination '.test-hooks')) | Out-Null
}

function Invoke-RegularAction([string]$ScriptDirectory, [string]$Action, [string]$Package, [string]$InstallRoot) {
    if ($Action -ceq 'install') {
        & (Join-Path $ScriptDirectory 'Install-LocalAgentHarness.ps1') -PackageRoot $Package -InstallRoot $InstallRoot
    } elseif ($Action -ceq 'update') {
        & (Join-Path $ScriptDirectory 'Update-LocalAgentHarness.ps1') -PackageRoot $Package -InstallRoot $InstallRoot
    } else {
        & (Join-Path $ScriptDirectory 'Uninstall-LocalAgentHarness.ps1') -InstallRoot $InstallRoot
    }
}

function Start-ChildAtBarrier([string]$ScriptDirectory, [string]$Action, [string]$Package, [string]$InstallRoot, [string]$Point) {
    $scriptPath = Join-Path $ScriptDirectory 'child.ps1'
    $escapedScript = $ScriptDirectory.Replace("'", "''")
    $escapedRoot = $InstallRoot.Replace("'", "''")
    $escapedPackage = $Package.Replace("'", "''")
    $body = if ($Action -ceq 'uninstall') {
        "`$ErrorActionPreference='Stop'; & '$escapedScript\Uninstall-LocalAgentHarness.ps1' -InstallRoot '$escapedRoot'"
    } elseif ($Action -ceq 'update') {
        "`$ErrorActionPreference='Stop'; & '$escapedScript\Update-LocalAgentHarness.ps1' -PackageRoot '$escapedPackage' -InstallRoot '$escapedRoot'"
    } else {
        "`$ErrorActionPreference='Stop'; & '$escapedScript\Install-LocalAgentHarness.ps1' -PackageRoot '$escapedPackage' -InstallRoot '$escapedRoot'"
    }
    [IO.File]::WriteAllText($scriptPath, $body, (New-Object Text.UTF8Encoding($false)))
    $hookRoot = Join-Path $ScriptDirectory '.test-hooks'
    $signal = Join-Path $hookRoot ($Point + '.ready')
    $stdout = Join-Path $ScriptDirectory 'child.stdout.txt'
    $stderr = Join-Path $ScriptDirectory 'child.stderr.txt'
    $exe = Join-Path $PSHOME 'powershell.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { $exe = Join-Path $PSHOME 'pwsh.exe' }
    $arguments = "-NoProfile -ExecutionPolicy Bypass -File `"$scriptPath`""
    $process = Start-Process -FilePath $exe -ArgumentList $arguments -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $deadline = [DateTime]::UtcNow.AddSeconds(45)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $signal -PathType Leaf) {
            try { Stop-Process -Id $process.Id -Force -ErrorAction Stop } catch { }
            $process.WaitForExit()
            return [pscustomobject]@{ ProcessId=$process.Id; Signal=$signal; Stdout=$stdout; Stderr=$stderr }
        }
        if ($process.HasExited) {
            $outText = if (Test-Path -LiteralPath $stdout) { [IO.File]::ReadAllText($stdout) } else { '' }
            $errText = if (Test-Path -LiteralPath $stderr) { [IO.File]::ReadAllText($stderr) } else { '' }
            throw "Child exited before deterministic barrier: $outText $errText"
        }
        Start-Sleep -Milliseconds 25
    }
    try { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue } catch { }
    throw 'Child did not reach the deterministic lifecycle barrier within 45 seconds.'
}

function Start-AbandonedMutexChild([string]$Directory, [string]$InstallRoot) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        $digest = $sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($InstallRoot.ToUpperInvariant()))
    } finally { $sha.Dispose() }
    $mutexName = 'Local\LAHReleaseLifecycle_' + [BitConverter]::ToString($digest).Replace('-','')
    $scriptPath = Join-Path $Directory 'mutex-holder.ps1'
    $signal = Join-Path $Directory 'mutex-holder.ready'
    $stdout = Join-Path $Directory 'mutex-holder.stdout.txt'
    $stderr = Join-Path $Directory 'mutex-holder.stderr.txt'
    $escapedName = $mutexName.Replace("'", "''")
    $escapedSignal = $signal.Replace("'", "''")
    $body = @"
`$ErrorActionPreference = 'Stop'
`$mutex = [Threading.Mutex]::new(`$false, '$escapedName')
`$null = `$mutex.WaitOne()
[IO.File]::WriteAllText('$escapedSignal', 'held')
while (`$true) { Start-Sleep -Milliseconds 100 }
"@
    [IO.File]::WriteAllText($scriptPath, $body, (New-Object Text.UTF8Encoding($false)))
    $exe = Join-Path $PSHOME 'powershell.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { $exe = Join-Path $PSHOME 'pwsh.exe' }
    $process = Start-Process -FilePath $exe -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File `"$scriptPath`"" -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $signal -PathType Leaf) {
            try { Stop-Process -Id $process.Id -Force -ErrorAction Stop } catch { }
            $process.WaitForExit()
            return $process.Id
        }
        if ($process.HasExited) { throw 'Mutex-holder child exited before it acquired the named lock.' }
        Start-Sleep -Milliseconds 25
    }
    try { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue } catch { }
    throw 'Mutex-holder child did not acquire the named lock in time.'
}

function Get-OwnedInventory([string]$InstallRoot) {
    $receipt = [IO.File]::ReadAllText((Join-Path $InstallRoot '.lah-install-receipt.json')) | ConvertFrom-Json
    $inventory = @{}
    foreach ($entry in @($receipt.files)) {
        $path = Join-Path $InstallRoot ([string]$entry.path)
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Missing owned file after recovery: $($entry.path)" }
        $item = Get-Item -LiteralPath $path -Force
        $hash = Get-Hash $path
        if ([long]$item.Length -ne [long]$entry.size -or $hash -cne [string]$entry.sha256) { throw "Owned file hash mismatch after recovery: $($entry.path)" }
        $inventory[[string]$entry.path] = $hash
    }
    return $inventory
}

function Get-FixtureSnapshot([string]$Root) {
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    $snapshot = @{}
    foreach ($item in @(Get-ChildItem -LiteralPath $fullRoot -Force -Recurse)) {
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'A recovery fixture unexpectedly contains a reparse point.' }
        $relative = $item.FullName.Substring($fullRoot.Length).TrimStart('\')
        if ($item.PSIsContainer) {
            $snapshot[$relative] = 'directory'
        } else {
            $snapshot[$relative] = ('file|{0}|{1}' -f [long]$item.Length, (Get-Hash $item.FullName))
        }
    }
    return $snapshot
}
function New-Fixture([string]$Name, [string]$PackageVariant) {
    $base = Join-Path $script:RunRoot $Name
    $scripts = Join-Path $base 'scripts'
    $install = Join-Path $base 'install-root'
    $package = Join-Path $base 'package-root'
    [IO.Directory]::CreateDirectory($base) | Out-Null
    [IO.Directory]::CreateDirectory($install) | Out-Null
    [IO.File]::WriteAllText((Join-Path $install 'user-marker.txt'), 'unknown user data retained')
    New-TestPackage -Destination $package -Variant $PackageVariant
    return [pscustomobject]@{ Base=$base; Scripts=$scripts; Install=$install; Package=$package }
}

function Assert-Marker([string]$InstallRoot, [string]$ExpectedHash) {
    $path = Join-Path $InstallRoot 'user-marker.txt'
    if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-Hash $path) -cne $ExpectedHash) { throw 'Unknown user data changed during lifecycle recovery.' }
}

function Assert-InvalidJournalSizeNoMutation([string]$Name, [object]$InjectedSize, [string]$ExpectedType) {
    $case = New-Fixture -Name $Name -PackageVariant 'size-one'
    $markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
    Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
    $newPackage = Join-Path $case.Base 'package-root-v2'
    New-TestPackage -Destination $newPackage -Variant 'v2'
    New-InstrumentedScripts -Destination $case.Scripts -BarrierPoint 'update-first-backup'
    $null = Start-ChildAtBarrier -ScriptDirectory $case.Scripts -Action 'update' -Package $newPackage -InstallRoot $case.Install -Point 'update-first-backup'

    $transactionRoots = @(Get-ChildItem -LiteralPath $case.Install -Force -Directory | Where-Object { $_.Name -like '.lah-transaction-*' })
    if ($transactionRoots.Count -ne 1) { throw 'The interrupted update did not leave exactly one owned transaction fixture.' }
    $journalPath = Join-Path $transactionRoots[0].FullName 'journal.json'
    $journal = [IO.File]::ReadAllText($journalPath) | ConvertFrom-Json
    $matches = @($journal.old_files | Where-Object { $_.path -ceq 'README.md' })
    $liveReadmePath = Join-Path $case.Install 'README.md'
    $backupRoot = Join-Path $transactionRoots[0].FullName 'backup'
    $backupReadmePath = Join-Path $backupRoot 'README.md'
    $readmeInRoot = Test-Path -LiteralPath $liveReadmePath
    $readmeInBackup = Test-Path -LiteralPath $backupReadmePath
    if ($readmeInRoot -eq $readmeInBackup) { throw 'The one-byte README must exist in exactly one owned lifecycle location.' }
    if ($readmeInBackup) {
        $backupRootItem = Get-Item -LiteralPath $backupRoot -Force
        if (-not $backupRootItem.PSIsContainer -or ($backupRootItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw 'The README transaction backup is not a direct physical test-owned directory.'
        }
    }
    $readmePath = if ($readmeInRoot) { $liveReadmePath } else { $backupReadmePath }
    $readmeItem = Get-Item -LiteralPath $readmePath -Force
    if ($readmeItem.PSIsContainer -or ($readmeItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $matches.Count -ne 1 -or $readmeItem.Length -ne 1 -or
        [long]$matches[0].size -ne 1 -or [string]$matches[0].sha256 -cne (Get-Hash $readmePath)) {
        throw 'The malformed-size fixture does not bind to an actual one-byte file and matching SHA-256.'
    }
    $matches[0].size = $InjectedSize
    [IO.File]::WriteAllText($journalPath, ($journal | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding($false)))

    $before = Get-FixtureSnapshot $case.Install
    $failedClosed = $false
    try {
        Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'update' -Package $newPackage -InstallRoot $case.Install | Out-Null
    } catch {
        if ($_.Exception.Message -notmatch 'Lifecycle recovery is required; no unverified files were changed') { throw }
        $failedClosed = $true
    }
    $after = Get-FixtureSnapshot $case.Install
    if (-not $failedClosed -or $before.Count -ne $after.Count) { throw "A $ExpectedType journal size did not fail closed with an unchanged full transaction inventory." }
    foreach ($path in $before.Keys) {
        if (-not $after.ContainsKey($path) -or $after[$path] -cne $before[$path]) {
            throw "A $ExpectedType journal size changed transaction or install-root data before rejection."
        }
    }
    Assert-Marker $case.Install $markerHash
    $results.Add([pscustomobject]@{ case="journal-size-$ExpectedType-matching-one-byte-file"; outcome='fixed-error-and-zero-mutation'; inventory_entries=$after.Count; marker_preserved=$true })
}

$script:PackageRoot = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $PackageRoot).Path)
$script:LifecycleSource = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $LifecycleSource).Path)
$script:PackageArchivePath = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $PackageArchivePath).Path)
$script:PackageArchiveSha256 = Assert-PackageArchiveBinding -ArchivePath $script:PackageArchivePath -ExpectedSha256 $ExpectedPackageArchiveSha256 -ExtractedRoot $script:PackageRoot -SourceRoot $script:LifecycleSource
[IO.Directory]::CreateDirectory($OutputRoot) | Out-Null
$script:RunRoot = [IO.Path]::GetFullPath((Join-Path $OutputRoot ('recovery-' + $PSVersionTable.PSVersion.ToString() + '-' + [Guid]::NewGuid().ToString('N'))))
Assert-WithinOwnerRoot $script:RunRoot $OutputRoot | Out-Null
[IO.Directory]::CreateDirectory($script:RunRoot) | Out-Null
$results = New-Object 'System.Collections.Generic.List[object]'

# First install interrupted after the first owned root payload move must roll forward.
$case = New-Fixture -Name 'install-kill' -PackageVariant 'v1'
$markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
New-InstrumentedScripts -Destination $case.Scripts -BarrierPoint 'install-first-payload'
$null = Start-ChildAtBarrier -ScriptDirectory $case.Scripts -Action 'install' -Package $case.Package -InstallRoot $case.Install -Point 'install-first-payload'
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
$installInventory = Get-OwnedInventory $case.Install
Assert-Marker $case.Install $markerHash
$results.Add([pscustomobject]@{ case='install-after-first-payload-move'; outcome='roll-forward-complete'; owned_count=$installInventory.Count; marker_preserved=$true })

# Update interrupted after receipt plus a payload backup move must restore the old set.
$case = New-Fixture -Name 'update-kill' -PackageVariant 'v1'
$markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
$beforeUpdate = Get-OwnedInventory $case.Install
$newPackage = Join-Path $case.Base 'package-root-v2'
New-TestPackage -Destination $newPackage -Variant 'v2'
New-InstrumentedScripts -Destination $case.Scripts -BarrierPoint 'update-first-backup'
$null = Start-ChildAtBarrier -ScriptDirectory $case.Scripts -Action 'update' -Package $newPackage -InstallRoot $case.Install -Point 'update-first-backup'
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
$afterRollback = Get-OwnedInventory $case.Install
foreach ($name in $beforeUpdate.Keys) { if ($afterRollback[$name] -cne $beforeUpdate[$name]) { throw 'Interrupted update did not restore the previous complete package before retry.' } }
Assert-Marker $case.Install $markerHash
$results.Add([pscustomobject]@{ case='update-after-old-receipt-and-first-payload-backup'; outcome='old-package-restored-before-retry'; owned_count=$afterRollback.Count; marker_preserved=$true })

# Uninstall interrupted before receipt move must restore the app before a re-install proceeds.
$case = New-Fixture -Name 'uninstall-precommit-kill' -PackageVariant 'v1'
$markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
New-InstrumentedScripts -Destination $case.Scripts -BarrierPoint 'uninstall-before-receipt'
$null = Start-ChildAtBarrier -ScriptDirectory $case.Scripts -Action 'uninstall' -Package $case.Package -InstallRoot $case.Install -Point 'uninstall-before-receipt'
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
$precommitInventory = Get-OwnedInventory $case.Install
Assert-Marker $case.Install $markerHash
$results.Add([pscustomobject]@{ case='uninstall-after-payload-before-receipt'; outcome='existing-package-restored-before-retry'; owned_count=$precommitInventory.Count; marker_preserved=$true })

# Uninstall interrupted after receipt commit must finish cleanup and return success on retry.
$case = New-Fixture -Name 'uninstall-postcommit-kill' -PackageVariant 'v1'
$markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
New-InstrumentedScripts -Destination $case.Scripts -BarrierPoint 'uninstall-after-receipt'
$null = Start-ChildAtBarrier -ScriptDirectory $case.Scripts -Action 'uninstall' -Package $case.Package -InstallRoot $case.Install -Point 'uninstall-after-receipt'
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'uninstall' -Package $case.Package -InstallRoot $case.Install | Out-Null
Assert-Marker $case.Install $markerHash
if (@(Get-ChildItem -LiteralPath $case.Install -Force).Count -ne 1) { throw 'Post-commit uninstall recovery left owned or transaction files behind.' }
$results.Add([pscustomobject]@{ case='uninstall-after-receipt-commit'; outcome='cleanup-completed-and-retry-succeeded'; owned_count=0; marker_preserved=$true })

# Malformed unexpected transaction data fails closed and remains byte-for-byte intact.
$case = New-Fixture -Name 'malformed-journal'
$markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
$transaction = Join-Path $case.Install ('.lah-transaction-' + ('a' * 32))
[IO.Directory]::CreateDirectory($transaction) | Out-Null
$unknown = Join-Path $transaction 'unknown-user-data.bin'
[IO.File]::WriteAllBytes($unknown, [byte[]](1,2,3,4,5))
$unknownHash = Get-Hash $unknown
$failedClosed = $false
try { Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null } catch {
    if ($_.Exception.Message -notmatch 'Lifecycle recovery is required; no unverified files were changed') { throw }
    $failedClosed = $true
}
if (-not $failedClosed -or (Get-Hash $unknown) -cne $unknownHash) { throw 'Malformed transaction was not preserved behind a fixed fail-closed result.' }
Assert-Marker $case.Install $markerHash
$results.Add([pscustomobject]@{ case='malformed-unexpected-transaction-file'; outcome='fixed-error-and-preserved'; unknown_hash=$unknownHash; marker_preserved=$true })

# A malformed second recognized transaction must be rejected before the valid first transaction changes anything.
$case = New-Fixture -Name 'preflight-multiple-transaction' -PackageVariant 'v1'
$markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
$newPackage = Join-Path $case.Base 'package-root-v2'
New-TestPackage -Destination $newPackage -Variant 'v2'
New-InstrumentedScripts -Destination $case.Scripts -BarrierPoint 'update-first-backup'
$null = Start-ChildAtBarrier -ScriptDirectory $case.Scripts -Action 'update' -Package $newPackage -InstallRoot $case.Install -Point 'update-first-backup'
$malformedTransaction = Join-Path $case.Install ('.lah-transaction-' + ('b' * 32))
[IO.Directory]::CreateDirectory($malformedTransaction) | Out-Null
[IO.File]::WriteAllBytes((Join-Path $malformedTransaction 'unrecognized.bin'), [byte[]](9,8,7,6))
$beforePreflight = Get-FixtureSnapshot $case.Install
$preflightRejected = $false
try {
    Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'update' -Package $newPackage -InstallRoot $case.Install | Out-Null
} catch {
    if ($_.Exception.Message -notmatch 'Lifecycle recovery is required; no unverified files were changed') { throw }
    $preflightRejected = $true
}
$afterPreflight = Get-FixtureSnapshot $case.Install
if (-not $preflightRejected -or $beforePreflight.Count -ne $afterPreflight.Count) { throw 'Multiple-transaction recovery did not fail closed with a stable full-tree inventory.' }
foreach ($path in $beforePreflight.Keys) {
    if (-not $afterPreflight.ContainsKey($path) -or $afterPreflight[$path] -cne $beforePreflight[$path]) {
        throw 'Recovery mutated a recognized transaction before rejecting a second malformed transaction.'
    }
}
Assert-Marker $case.Install $markerHash
$results.Add([pscustomobject]@{ case='valid-interrupted-update-plus-malformed-second-transaction'; outcome='fixed-error-and-zero-mutation'; inventory_entries=$afterPreflight.Count; marker_preserved=$true })

# JSON strings and booleans that numerically coerce to the actual size must be rejected before recovery moves files.
Assert-InvalidJournalSizeNoMutation -Name 'journal-size-string' -InjectedSize '1' -ExpectedType 'string'
Assert-InvalidJournalSizeNoMutation -Name 'journal-size-boolean' -InjectedSize ([bool]$true) -ExpectedType 'boolean'

# An abandoned named mutex is owned by the waiter; recovery must use and release it.
$case = New-Fixture -Name 'abandoned-mutex'
$markerHash = Get-Hash (Join-Path $case.Install 'user-marker.txt')
$null = Start-AbandonedMutexChild -Directory $case.Base -InstallRoot $case.Install
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'install' -Package $case.Package -InstallRoot $case.Install | Out-Null
$abandonedInventory = Get-OwnedInventory $case.Install
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'update' -Package $case.Package -InstallRoot $case.Install | Out-Null
Invoke-RegularAction -ScriptDirectory $script:LifecycleSource -Action 'uninstall' -Package $case.Package -InstallRoot $case.Install | Out-Null
Assert-Marker $case.Install $markerHash
$results.Add([pscustomobject]@{ case='abandoned-mutex-recovery-and-release'; outcome='recovered-then-relocked-and-released'; owned_count=$abandonedInventory.Count; marker_preserved=$true })

$report = [ordered]@{
    schema='lah-lifecycle-recovery-test-report-v1'
    runtime=$PSVersionTable.PSVersion.ToString()
    edition=$PSVersionTable.PSEdition
    source_module_sha256=(Get-Hash (Join-Path $script:LifecycleSource 'LocalAgentHarness.Lifecycle.psm1'))
    package_zip_path=$script:PackageArchivePath
    package_zip_sha256=$script:PackageArchiveSha256
    package_root=$script:PackageRoot
    lifecycle_source_root=$script:LifecycleSource
    run_root=$script:RunRoot
    checks=@($results.ToArray())
}
$reportPath = Join-Path $script:RunRoot 'report.json'
[IO.File]::WriteAllText($reportPath, ($report | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
Write-Output ("Lifecycle recovery acceptance passed: {0} checks under PowerShell {1}. Report: {2}" -f $results.Count, $report.runtime, $reportPath)
