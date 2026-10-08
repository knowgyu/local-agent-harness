Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:LAHProduct = 'local-agent-harness'
$script:LAHReceiptName = '.lah-install-receipt.json'
$script:LAHPayloadNames = @(
    'local-agent-harness.exe',
    'README.md',
    'CLIENT_SETUP.md',
    'Install-LocalAgentHarness.ps1',
    'Update-LocalAgentHarness.ps1',
    'Uninstall-LocalAgentHarness.ps1',
    'LocalAgentHarness.Lifecycle.psm1'
)
$script:LAHChecksumNames = @($script:LAHPayloadNames + 'manifest.json')
$script:LAHInstallNames = @($script:LAHPayloadNames + 'manifest.json' + 'SHA256SUMS.txt')
$script:LAHAllowedNames = @($script:LAHInstallNames)

function Test-LAHNonNegativeJsonInteger {
    param([AllowNull()][object]$Value)
    if ($null -eq $Value) { return $false }
    $valueType = $Value.GetType()
    if ($valueType -ne [System.Int32] -and $valueType -ne [System.Int64]) { return $false }
    return ([long]$Value -ge 0)
}

function Get-LAHDefaultInstallRoot {
    return (Resolve-LAHDefaultInstallRootFrom -LocalAppData $env:LOCALAPPDATA)
}

function Resolve-LAHDefaultInstallRootFrom {
    [CmdletBinding()]
    param([AllowNull()][AllowEmptyString()][string]$LocalAppData)
    if ([string]::IsNullOrWhiteSpace($LocalAppData)) {
        throw 'LOCALAPPDATA is unavailable; pass an explicit user-local install root.'
    }
    $base = Resolve-LAHPath -Path $LocalAppData -Label 'LOCALAPPDATA'
    return (Join-Path $base 'Programs\Local Agent Harness')
}

function Resolve-LAHPath {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Label)
    if ([string]::IsNullOrWhiteSpace($Path) -or $Path -notmatch '^[A-Za-z]:\\' -or -not [IO.Path]::IsPathRooted($Path)) {
        throw "$Label must be an absolute local Windows path."
    }
    $full = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    if ($full -notmatch '^[A-Za-z]:\\' -or $full -match '^[A-Za-z]:$') {
        throw "$Label must name a directory on a local drive."
    }
    return $full
}

function Assert-LAHNoReparsePath {
    param([Parameter(Mandatory)][string]$Path)
    $full = [IO.Path]::GetFullPath($Path)
    $drive = [IO.Path]::GetPathRoot($full)
    if ([string]::IsNullOrEmpty($drive) -or $drive.StartsWith('\\')) {
        throw 'Only local drive paths are supported.'
    }
    $relative = $full.Substring($drive.Length).Split('\', [StringSplitOptions]::RemoveEmptyEntries)
    $current = $drive
    foreach ($part in $relative) {
        $current = Join-Path $current $part
        if (Test-Path -LiteralPath $current) {
            $item = Get-Item -LiteralPath $current -Force
            if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw "Reparse points are not allowed in managed paths: $current"
            }
        }
    }
}

function Open-LAHRootLease {
    param([Parameter(Mandatory)][string]$InstallRoot)
    Assert-LAHNoReparsePath -Path $InstallRoot
    if (-not ('LAHLifecycleNative' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
[StructLayout(LayoutKind.Sequential)]
public struct LAHByHandleFileInformation {
    public uint FileAttributes;
    public uint CreationTimeLow;
    public uint CreationTimeHigh;
    public uint LastAccessTimeLow;
    public uint LastAccessTimeHigh;
    public uint LastWriteTimeLow;
    public uint LastWriteTimeHigh;
    public uint VolumeSerialNumber;
    public uint FileSizeHigh;
    public uint FileSizeLow;
    public uint NumberOfLinks;
    public uint FileIndexHigh;
    public uint FileIndexLow;
}
public static class LAHLifecycleNative {
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
    public static extern SafeFileHandle CreateFile(string name, uint access, uint share, IntPtr security, uint creation, uint flags, IntPtr template);
    [DllImport("kernel32.dll", SetLastError=true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool GetFileInformationByHandle(SafeFileHandle handle, out LAHByHandleFileInformation information);
}
'@
    }
    # FILE_LIST_DIRECTORY is the access bit that blocked root rename in PS7/PS5.1 probes.
    # Share read/write only; deliberately omit FILE_SHARE_DELETE.
    $handle = [LAHLifecycleNative]::CreateFile($InstallRoot, 0x1, 0x3, [IntPtr]::Zero, 3, 0x02000000 -bor 0x00200000, [IntPtr]::Zero)
    if ($handle.IsInvalid) {
        $handle.Dispose()
        throw 'The named install root could not be held safely for this lifecycle operation.'
    }
    $information = New-Object LAHByHandleFileInformation
    if (-not [LAHLifecycleNative]::GetFileInformationByHandle($handle, [ref]$information)) {
        $handle.Dispose()
        throw 'The named install root could not be verified safely for this lifecycle operation.'
    }
    if (($information.FileAttributes -band 0x10) -eq 0 -or ($information.FileAttributes -band 0x400) -ne 0) {
        $handle.Dispose()
        throw 'The named install root is not a plain directory.'
    }
    # Recheck the lexical path after opening the handle. OPEN_REPARSE_POINT makes a
    # substituted root junction visible in the handle attributes above.
    try {
        Assert-LAHNoReparsePath -Path $InstallRoot
        if (-not (Test-Path -LiteralPath $InstallRoot -PathType Container)) {
            throw 'The named install root changed while it was being opened.'
        }
        $pathHandle = [LAHLifecycleNative]::CreateFile($InstallRoot, 0x80, 0x7, [IntPtr]::Zero, 3, 0x02000000 -bor 0x00200000, [IntPtr]::Zero)
        if ($pathHandle.IsInvalid) { $pathHandle.Dispose(); throw 'The named install root changed while it was being opened.' }
        try {
            $pathInformation = New-Object LAHByHandleFileInformation
            if (-not [LAHLifecycleNative]::GetFileInformationByHandle($pathHandle, [ref]$pathInformation) -or
                $pathInformation.VolumeSerialNumber -ne $information.VolumeSerialNumber -or
                $pathInformation.FileIndexHigh -ne $information.FileIndexHigh -or
                $pathInformation.FileIndexLow -ne $information.FileIndexLow -or
                ($pathInformation.FileAttributes -band 0x400) -ne 0) {
                throw 'The named install root changed while it was being opened.'
            }
        } finally { $pathHandle.Dispose() }
    } catch {
        $handle.Dispose()
        throw
    }
    return $handle
}

function Get-LAHHash {
    param([Parameter(Mandatory)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Enter-LAHOperationMutex {
    param([Parameter(Mandatory)][string]$InstallRoot)
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        $digest = $sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($InstallRoot.ToUpperInvariant()))
    } finally {
        $sha.Dispose()
    }
    $name = 'Local\LAHReleaseLifecycle_' + [BitConverter]::ToString($digest).Replace('-', '')
    $mutex = [Threading.Mutex]::new($false, $name)
    try {
        $acquired = $mutex.WaitOne(0)
    } catch [Threading.AbandonedMutexException] {
        # .NET reports an abandoned mutex after granting ownership to this thread.
        # Keep that ownership so journal recovery can run under the same lock.
        $acquired = $true
    }
    if (-not $acquired) {
        $mutex.Dispose()
        throw 'Another install, update, or uninstall is already working on this named root.'
    }
    return $mutex
}

function Get-LAHFileRecord {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Name)
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer -or (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)) {
        throw "Expected a regular package file: $Name"
    }
    return [pscustomobject]@{
        path = $Name
        size = [long]$item.Length
        sha256 = Get-LAHHash -Path $Path
    }
}

function Read-LAHPackage {
    param([Parameter(Mandatory)][string]$PackageRoot)
    $root = Resolve-LAHPath -Path $PackageRoot -Label 'PackageRoot'
    if (-not (Test-Path -LiteralPath $root -PathType Container)) {
        throw 'The package directory does not exist.'
    }
    Assert-LAHNoReparsePath -Path $root

    $expectedPackageFiles = @($script:LAHInstallNames)
    $actualFiles = @(Get-ChildItem -LiteralPath $root -Force -File | ForEach-Object Name)
    $actualDirs = @(Get-ChildItem -LiteralPath $root -Force -Directory | ForEach-Object Name)
    if ($actualDirs.Count -ne 0 -or $actualFiles.Count -ne $expectedPackageFiles.Count) {
        throw 'The package must contain only the documented root-level files.'
    }
    foreach ($name in $expectedPackageFiles) {
        if ($actualFiles -cnotcontains $name) { throw "The package is missing required file: $name" }
    }
    foreach ($name in $actualFiles) {
        if ($expectedPackageFiles -cnotcontains $name) { throw "The package contains an unexpected file: $name" }
    }

    $sumsPath = Join-Path $root 'SHA256SUMS.txt'
    $sumLines = [IO.File]::ReadAllLines($sumsPath)
    $checksums = @{}
    foreach ($line in $sumLines) {
        if ($line -notmatch '^([0-9a-f]{64})  ([A-Za-z0-9][A-Za-z0-9._-]*)$') {
            throw 'SHA256SUMS.txt has an invalid line.'
        }
        $name = $Matches[2]
        if ($checksums.ContainsKey($name)) { throw "SHA256SUMS.txt repeats file: $name" }
        $checksums[$name] = $Matches[1]
    }
    if ($checksums.Count -ne $script:LAHChecksumNames.Count) {
        throw 'SHA256SUMS.txt does not list the exact package payload.'
    }
    foreach ($name in $script:LAHChecksumNames) {
        if (-not $checksums.ContainsKey($name)) { throw "SHA256SUMS.txt does not list $name" }
        $filePath = Join-Path $root $name
        Assert-LAHNoReparsePath -Path $filePath
        $actualHash = Get-LAHHash -Path $filePath
        if ($actualHash -cne $checksums[$name]) { throw "Package checksum failed for $name" }
    }

    try {
        $manifest = [IO.File]::ReadAllText((Join-Path $root 'manifest.json')) | ConvertFrom-Json
    } catch {
        throw 'manifest.json is not valid JSON.'
    }
    if ($manifest.schema -cne 'lah-portable-package-manifest-v1') {
        throw 'manifest.json has an unsupported schema.'
    }
    if ([string]$manifest.product -cne $script:LAHProduct) {
        throw 'manifest.json identifies a different product.'
    }
    if ($null -eq $manifest.files) { throw 'manifest.json is missing its file list.' }
    $manifestFiles = @{}
    foreach ($entry in @($manifest.files)) {
        $name = [string]$entry.path
        if ($script:LAHPayloadNames -cnotcontains $name -or $manifestFiles.ContainsKey($name)) {
            throw 'manifest.json has an invalid or repeated payload path.'
        }
        if (-not (Test-LAHNonNegativeJsonInteger -Value $entry.size) -or [string]$entry.sha256 -cnotmatch '^[0-9a-f]{64}$') {
            throw "manifest.json has invalid file metadata for $name"
        }
        $manifestFiles[$name] = $entry
    }
    if ($manifestFiles.Count -ne $script:LAHPayloadNames.Count) {
        throw 'manifest.json does not list the exact payload files.'
    }
    $records = @{}
    foreach ($name in $script:LAHInstallNames) {
        $record = Get-LAHFileRecord -Path (Join-Path $root $name) -Name $name
        $records[$name] = $record
        if ($script:LAHPayloadNames -ccontains $name) {
            $entry = $manifestFiles[$name]
            if ([long]$entry.size -ne $record.size -or [string]$entry.sha256 -cne $record.sha256) {
                throw "manifest.json does not match package file $name"
            }
        }
    }
    return [pscustomobject]@{ Root = $root; Files = $records }
}

function Read-LAHInstallReceipt {
    param([Parameter(Mandatory)][string]$InstallRoot, [switch]$AllowMissingFiles)
    $path = Join-Path $InstallRoot $script:LAHReceiptName
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { return $null }
    Assert-LAHNoReparsePath -Path $path
    try {
        $receipt = [IO.File]::ReadAllText($path) | ConvertFrom-Json
    } catch {
        throw 'The install receipt is invalid JSON; no files were changed.'
    }
    if ($receipt.schema -cne 'lah-user-install-receipt-v1' -or [string]$receipt.product -cne $script:LAHProduct) {
        throw 'The install receipt does not identify this product; no files were changed.'
    }
    $receiptRoot = Resolve-LAHPath -Path ([string]$receipt.install_root) -Label 'Receipt install_root'
    if (-not [string]::Equals($receiptRoot, $InstallRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'The install receipt belongs to a different named root; no files were changed.'
    }
    $map = @{}
    foreach ($entry in @($receipt.files)) {
        $name = [string]$entry.path
        if ($script:LAHAllowedNames -cnotcontains $name -or $map.ContainsKey($name)) {
            throw 'The install receipt contains an unowned or repeated path; no files were changed.'
        }
        if (-not (Test-LAHNonNegativeJsonInteger -Value $entry.size) -or [string]$entry.sha256 -cnotmatch '^[0-9a-f]{64}$') {
            throw "The install receipt has invalid metadata for $name; no files were changed."
        }
        $map[$name] = $entry
    }
    if ($map.Count -eq 0) { throw 'The install receipt lists no owned files; no files were changed.' }
    foreach ($name in $map.Keys) {
        $filePath = Join-Path $InstallRoot $name
        if (-not (Test-Path -LiteralPath $filePath -PathType Leaf)) {
            if ($AllowMissingFiles) { continue }
            throw "An owned file is missing: $name; no files were changed."
        }
        Assert-LAHNoReparsePath -Path $filePath
        $record = Get-LAHFileRecord -Path $filePath -Name $name
        if ($record.size -ne [long]$map[$name].size -or $record.sha256 -cne [string]$map[$name].sha256) {
            throw "An owned file has changed: $name; no files were changed."
        }
    }
    return [pscustomobject]@{ Path = $path; Files = $map }
}

function Assert-LAHNotRunning {
    param([Parameter(Mandatory)][string]$InstallRoot)
    $exePath = Join-Path $InstallRoot 'local-agent-harness.exe'
    if (-not (Test-Path -LiteralPath $exePath -PathType Leaf)) { return }
    $processes = @(Get-Process -Name 'local-agent-harness' -ErrorAction SilentlyContinue)
    foreach ($process in $processes) {
        try {
            $processPath = $process.MainModule.FileName
        } catch {
            throw 'A process with the application name is running and its path cannot be checked. Close it before continuing.'
        }
        if ([string]::Equals([IO.Path]::GetFullPath($processPath), $exePath, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'The installed application is running. Close it before continuing.'
        }
    }
}

function New-LAHReceiptText {
    param([Parameter(Mandatory)][string]$InstallRoot, [Parameter(Mandatory)][hashtable]$Files)
    $list = @($Files.Keys | Sort-Object | ForEach-Object {
        [pscustomobject]@{
            path = $_
            size = [long]$Files[$_].size
            sha256 = [string]$Files[$_].sha256
        }
    })
    $receipt = [ordered]@{
        schema = 'lah-user-install-receipt-v1'
        product = $script:LAHProduct
        install_root = $InstallRoot
        files = $list
    }
    return ($receipt | ConvertTo-Json -Depth 6)
}

function Remove-LAHTransaction {
    param([Parameter(Mandatory)][string]$TransactionRoot, [Parameter(Mandatory)][string[]]$AllowedRelativePaths)
    if (-not (Test-Path -LiteralPath $TransactionRoot -PathType Container)) { return }
    Assert-LAHNoReparsePath -Path $TransactionRoot
    $base = $TransactionRoot.TrimEnd('\') + '\'
    foreach ($child in @(Get-ChildItem -LiteralPath $TransactionRoot -Force)) {
        if ($child.PSIsContainer) {
            if (@('new','backup') -cnotcontains $child.Name -or (($child.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)) {
                throw 'Unexpected data exists in a lifecycle transaction directory; it was preserved.'
            }
            foreach ($nested in @(Get-ChildItem -LiteralPath $child.FullName -Force)) {
                $relative = $nested.FullName.Substring($base.Length)
                if ($nested.PSIsContainer -or (($nested.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) -or $AllowedRelativePaths -cnotcontains $relative) {
                    throw 'Unexpected data exists in a lifecycle transaction directory; it was preserved.'
                }
                Remove-Item -LiteralPath $nested.FullName -Force
            }
            Remove-Item -LiteralPath $child.FullName -Force
        } else {
            $relative = $child.FullName.Substring($base.Length)
            if (($child.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or $AllowedRelativePaths -cnotcontains $relative) {
                throw 'Unexpected data exists in a lifecycle transaction directory; it was preserved.'
            }
            Remove-Item -LiteralPath $child.FullName -Force
        }
    }
    Remove-Item -LiteralPath $TransactionRoot -Force
}

function Write-LAHTransactionJournal {
    param([Parameter(Mandatory)][string]$TransactionRoot, [Parameter(Mandatory)]$Journal)
    $journalPath = Join-Path $TransactionRoot 'journal.json'
    $nextPath = Join-Path $TransactionRoot 'journal.next'
    $previousPath = Join-Path $TransactionRoot 'journal.previous'
    if (Test-Path -LiteralPath $nextPath) {
        $nextItem = Get-Item -LiteralPath $nextPath -Force
        if ($nextItem.PSIsContainer -or (($nextItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        Remove-Item -LiteralPath $nextPath -Force
    }
    $text = $Journal | ConvertTo-Json -Depth 10
    [IO.File]::WriteAllText($nextPath, $text, (New-Object Text.UTF8Encoding($false)))
    if (Test-Path -LiteralPath $journalPath -PathType Leaf) {
        if (Test-Path -LiteralPath $previousPath) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        [IO.File]::Replace($nextPath, $journalPath, $previousPath)
        Remove-Item -LiteralPath $previousPath -Force
    } else {
        [IO.File]::Move($nextPath, $journalPath)
    }
}

function Test-LAHRecordAtPath {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)]$Record)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    try {
        if (-not (Test-LAHNonNegativeJsonInteger -Value $Record.size)) { return $false }
        Assert-LAHNoReparsePath -Path $Path
        $actual = Get-LAHFileRecord -Path $Path -Name ([string]$Record.path)
        return ($actual.size -eq [long]$Record.size -and $actual.sha256 -ceq [string]$Record.sha256)
    } catch { return $false }
}

function ConvertTo-LAHRecordMap {
    param([AllowNull()][object[]]$Records, [Parameter(Mandatory)][string[]]$AllowedNames)
    $map = @{}
    foreach ($record in @($Records)) {
        if ($null -eq $record) { continue }
        $name = [string]$record.path
        $recordProperties = @($record.PSObject.Properties.Name | Sort-Object)
        if ($recordProperties.Count -ne 3 -or $recordProperties -cnotcontains 'path' -or $recordProperties -cnotcontains 'size' -or $recordProperties -cnotcontains 'sha256' -or
            $AllowedNames -cnotcontains $name -or $map.ContainsKey($name) -or -not (Test-LAHNonNegativeJsonInteger -Value $record.size) -or [string]$record.sha256 -cnotmatch '^[0-9a-f]{64}$') {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        $map[$name] = $record
    }
    return $map
}

function Read-LAHTransactionJournal {
    param([Parameter(Mandatory)][string]$TransactionRoot, [Parameter(Mandatory)][string]$InstallRoot)
    $leaf = [IO.Path]::GetFileName($TransactionRoot)
    $path = Join-Path $TransactionRoot 'journal.json'
    $next = Join-Path $TransactionRoot 'journal.next'
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        if (-not (Test-Path -LiteralPath $next -PathType Leaf)) { return $null }
        $path = $next
    }
    Assert-LAHNoReparsePath -Path $path
    $journalItem = Get-Item -LiteralPath $path -Force
    if ($journalItem.PSIsContainer -or $journalItem.Length -gt 1048576) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    try { $journal = Read-LAHBoundedTextFile -Path $path | ConvertFrom-Json } catch {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    $expectedProperties = @('schema','operation','phase','install_root','transaction_name','old_receipt','old_files','old_present','new_receipt','new_files') | Sort-Object
    $actualProperties = @($journal.PSObject.Properties.Name | Sort-Object)
    if ($actualProperties.Count -ne $expectedProperties.Count -or @((Compare-Object -ReferenceObject $expectedProperties -DifferenceObject $actualProperties -CaseSensitive)).Count -ne 0 -or
        $journal.schema -cne 'lah-lifecycle-journal-v1' -or [string]$journal.transaction_name -cne $leaf) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    try { $journalRoot = Resolve-LAHPath -Path ([string]$journal.install_root) -Label 'journal install_root' } catch {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    if (-not [string]::Equals($journalRoot, $InstallRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    if (@('install','update','uninstall') -cnotcontains [string]$journal.operation -or @('staging','ready','applying','committed') -cnotcontains [string]$journal.phase) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    $auxiliaryPaths = @(@((Join-Path $TransactionRoot 'journal.next'), (Join-Path $TransactionRoot 'journal.previous')) | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf })
    if ($auxiliaryPaths.Count -gt 1 -or ($path -ceq $next -and $auxiliaryPaths.Count -gt 0)) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    foreach ($auxiliaryPath in $auxiliaryPaths) {
        Assert-LAHNoReparsePath -Path $auxiliaryPath
        $auxiliaryItem = Get-Item -LiteralPath $auxiliaryPath -Force
        if ($auxiliaryItem.PSIsContainer -or $auxiliaryItem.Length -gt 1048576) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        try { $auxiliary = Read-LAHBoundedTextFile -Path $auxiliaryPath | ConvertFrom-Json } catch {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        $auxiliaryProperties = @($auxiliary.PSObject.Properties.Name | Sort-Object)
        if ($auxiliaryProperties.Count -ne $expectedProperties.Count -or @((Compare-Object -ReferenceObject $expectedProperties -DifferenceObject $auxiliaryProperties -CaseSensitive)).Count -ne 0) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        try { $auxiliaryRoot = Resolve-LAHPath -Path ([string]$auxiliary.install_root) -Label 'journal install_root' } catch {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        if ($auxiliary.schema -cne 'lah-lifecycle-journal-v1' -or [string]$auxiliary.transaction_name -cne $leaf -or
            [string]$auxiliary.operation -cne [string]$journal.operation -or
            -not [string]::Equals($auxiliaryRoot, $InstallRoot, [StringComparison]::OrdinalIgnoreCase) -or
            @('staging','ready','applying','committed') -cnotcontains [string]$auxiliary.phase) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        $mainComparable = $journal | Select-Object schema,operation,install_root,transaction_name,old_receipt,old_files,old_present,new_receipt,new_files | ConvertTo-Json -Depth 10 -Compress
        $auxComparable = $auxiliary | Select-Object schema,operation,install_root,transaction_name,old_receipt,old_files,old_present,new_receipt,new_files | ConvertTo-Json -Depth 10 -Compress
        if ($mainComparable -cne $auxComparable) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        $allowedTransitions = @(
            @('staging','staging'), @('staging','ready'), @('ready','ready'), @('ready','committed'),
            @('committed','committed'), @('applying','applying')
        )
        $validTransition = $false
        foreach ($transition in $allowedTransitions) {
            if (([string]$journal.phase -ceq $transition[0] -and [string]$auxiliary.phase -ceq $transition[1]) -or
                ([string]$journal.phase -ceq $transition[1] -and [string]$auxiliary.phase -ceq $transition[0])) { $validTransition = $true; break }
        }
        if (-not $validTransition) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    }
    $journal | Add-Member -NotePropertyName JournalPath -NotePropertyValue $path -Force
    return $journal
}

function Assert-LAHTransactionLayout {
    param([Parameter(Mandatory)][string]$TransactionRoot, [Parameter(Mandatory)]$Journal)
    if ($Journal.old_files -isnot [Array] -or $Journal.new_files -isnot [Array] -or $Journal.old_present -isnot [Array]) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    $operation = [string]$Journal.operation
    $phase = [string]$Journal.phase
    $isUninstall = $operation -ceq 'uninstall'
    $newMap = ConvertTo-LAHRecordMap -Records @($Journal.new_files) -AllowedNames $script:LAHInstallNames
    $oldMap = ConvertTo-LAHRecordMap -Records @($Journal.old_files) -AllowedNames $script:LAHInstallNames
    $oldPresent = @($Journal.old_present | ForEach-Object {
        if ($_ -isnot [string]) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        [string]$_
    })
    $uniqueOldPresent = @($oldPresent | Select-Object -Unique)
    if ($uniqueOldPresent.Count -ne $oldPresent.Count -or @($oldPresent | Where-Object { $script:LAHInstallNames -cnotcontains $_ }).Count -ne 0) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    foreach ($name in $oldPresent) { if (-not $oldMap.ContainsKey($name)) { throw 'Lifecycle recovery is required; no unverified files were changed.' } }

    foreach ($pair in @(@{record=$Journal.old_receipt; required=($operation -cne 'install')}, @{record=$Journal.new_receipt; required=($operation -cne 'uninstall')})) {
        $record = $pair.record
        if ($pair.required -and $null -eq $record) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        if ($null -eq $record) { continue }
        $recordProperties = @($record.PSObject.Properties.Name | Sort-Object)
        $expectedRecordProperties = @('path','size','sha256') | Sort-Object
        if ($recordProperties.Count -ne $expectedRecordProperties.Count -or
            @((Compare-Object -ReferenceObject $expectedRecordProperties -DifferenceObject $recordProperties -CaseSensitive)).Count -ne 0 -or
            [string]$record.path -cne $script:LAHReceiptName -or -not (Test-LAHNonNegativeJsonInteger -Value $record.size) -or
            [string]$record.sha256 -cnotmatch '^[0-9a-f]{64}$') {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
    }

    switch ($operation) {
        'install' {
            if (($null -eq $Journal.old_receipt -and ($oldMap.Count -ne 0 -or $oldPresent.Count -ne 0)) -or
                ($null -ne $Journal.old_receipt -and $oldMap.Count -eq 0) -or
                $newMap.Count -ne $script:LAHInstallNames.Count -or
                @($script:LAHInstallNames | Where-Object { -not $newMap.ContainsKey($_) }).Count -ne 0 -or
                @('staging','ready','applying') -cnotcontains $phase) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
        }
        'update' {
            if ($oldMap.Count -eq 0 -or $newMap.Count -ne $script:LAHInstallNames.Count -or
                @($script:LAHInstallNames | Where-Object { -not $newMap.ContainsKey($_) }).Count -ne 0 -or
                @('staging','ready','applying') -cnotcontains $phase) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
        }
        'uninstall' {
            if ($oldMap.Count -eq 0 -or $newMap.Count -ne 0 -or
                @('ready','committed') -cnotcontains $phase) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
        }
        default { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    }

    $allowedTopFiles = @('journal.json','journal.next','journal.previous')
    $allowedNested = @{}
    if ($isUninstall) {
        $allowedTopFiles += @($oldPresent + $script:LAHReceiptName)
    } else {
        $allowedNested['new'] = @($newMap.Keys + $script:LAHReceiptName)
        $allowedNested['backup'] = @($oldPresent + $(if ($null -ne $Journal.old_receipt) { $script:LAHReceiptName }))
    }
    foreach ($child in @(Get-ChildItem -LiteralPath $TransactionRoot -Force)) {
        if (($child.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        if ($child.PSIsContainer) {
            if ($isUninstall -or @('new','backup') -cnotcontains $child.Name) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            Assert-LAHNoReparsePath -Path $child.FullName
            foreach ($nested in @(Get-ChildItem -LiteralPath $child.FullName -Force)) {
                if ($nested.PSIsContainer -or ($allowedNested[$child.Name] -cnotcontains $nested.Name) -or (($nested.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
        } elseif ($allowedTopFiles -cnotcontains $child.Name) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
    }
    if (-not $isUninstall) {
        foreach ($pair in @(@{dir='new';map=$newMap;receipt=$Journal.new_receipt}, @{dir='backup';map=$oldMap;receipt=$Journal.old_receipt})) {
            $directory = Join-Path $TransactionRoot $pair.dir
            if (-not (Test-Path -LiteralPath $directory -PathType Container)) { continue }
            foreach ($file in @(Get-ChildItem -LiteralPath $directory -Force -File)) {
                if ($file.Name -ceq $script:LAHReceiptName) {
                    $record = $pair.receipt
                } else { $record = $pair.map[$file.Name] }
                if ($null -eq $record -or -not (Test-LAHRecordAtPath -Path $file.FullName -Record $record)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
        }
    } else {
        foreach ($file in @(Get-ChildItem -LiteralPath $TransactionRoot -Force -File | Where-Object { @($script:LAHInstallNames + $script:LAHReceiptName) -ccontains $_.Name })) {
            $record = if ($file.Name -ceq $script:LAHReceiptName) { $Journal.old_receipt } else { $oldMap[$file.Name] }
            if ($null -eq $record -or -not (Test-LAHRecordAtPath -Path $file.FullName -Record $record)) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
        }
    }

    $rootReceiptPath = Join-Path ([string]$Journal.install_root) $script:LAHReceiptName
    $receiptCandidates = @(
        $rootReceiptPath,
        (Join-Path $TransactionRoot $script:LAHReceiptName),
        (Join-Path (Join-Path $TransactionRoot 'new') $script:LAHReceiptName),
        (Join-Path (Join-Path $TransactionRoot 'backup') $script:LAHReceiptName)
    )
    foreach ($candidate in $receiptCandidates) {
        if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) { continue }
        $candidateRecord = Get-LAHFileRecord -Path $candidate -Name $script:LAHReceiptName
        if ($null -ne $Journal.old_receipt -and
            [long]$candidateRecord.size -eq [long]$Journal.old_receipt.size -and
            [string]$candidateRecord.sha256 -ceq [string]$Journal.old_receipt.sha256) {
            Assert-LAHReceiptMatchesMap -Path $candidate -ReceiptRecord $Journal.old_receipt -InstallRoot ([string]$Journal.install_root) -ExpectedFiles $oldMap
        } elseif ($null -ne $Journal.new_receipt -and
            [long]$candidateRecord.size -eq [long]$Journal.new_receipt.size -and
            [string]$candidateRecord.sha256 -ceq [string]$Journal.new_receipt.sha256) {
            Assert-LAHReceiptMatchesMap -Path $candidate -ReceiptRecord $Journal.new_receipt -InstallRoot ([string]$Journal.install_root) -ExpectedFiles $newMap
        } else {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
    }
}

function Complete-LAHApplyTransactionCleanup {
    param([Parameter(Mandatory)][string]$TransactionRoot)
    foreach ($directoryName in @('new','backup')) {
        $directory = Join-Path $TransactionRoot $directoryName
        if (-not (Test-Path -LiteralPath $directory -PathType Container)) { continue }
        Assert-LAHNoReparsePath -Path $directory
        foreach ($file in @(Get-ChildItem -LiteralPath $directory -Force -File)) {
            if ($script:LAHInstallNames -cnotcontains $file.Name -and $file.Name -cne $script:LAHReceiptName) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            if (($file.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            Remove-Item -LiteralPath $file.FullName -Force
        }
        if (@(Get-ChildItem -LiteralPath $directory -Force).Count -ne 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        Remove-Item -LiteralPath $directory -Force
    }
    foreach ($name in @('journal.next','journal.previous','journal.json')) {
        $path = Join-Path $TransactionRoot $name
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            Assert-LAHNoReparsePath -Path $path
            Remove-Item -LiteralPath $path -Force
        }
    }
    if (@(Get-ChildItem -LiteralPath $TransactionRoot -Force).Count -ne 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    Remove-Item -LiteralPath $TransactionRoot -Force
}

function Complete-LAHUninstallTransactionCleanup {
    param([Parameter(Mandatory)][string]$TransactionRoot, [Parameter(Mandatory)]$Journal)
    $oldMap = ConvertTo-LAHRecordMap -Records @($Journal.old_files) -AllowedNames $script:LAHInstallNames
    foreach ($name in $Journal.old_present) {
        $path = Join-Path $TransactionRoot $name
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { continue }
        if (-not (Test-LAHRecordAtPath -Path $path -Record $oldMap[$name])) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        Remove-Item -LiteralPath $path -Force
    }
    $receiptPath = Join-Path $TransactionRoot $script:LAHReceiptName
    if (Test-Path -LiteralPath $receiptPath -PathType Leaf) {
        if ($null -eq $Journal.old_receipt -or -not (Test-LAHRecordAtPath -Path $receiptPath -Record $Journal.old_receipt)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        Remove-Item -LiteralPath $receiptPath -Force
    }
    foreach ($name in @('journal.next','journal.previous','journal.json')) {
        $path = Join-Path $TransactionRoot $name
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            Assert-LAHNoReparsePath -Path $path
            Remove-Item -LiteralPath $path -Force
        }
    }
    if (@(Get-ChildItem -LiteralPath $TransactionRoot -Force).Count -ne 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    Remove-Item -LiteralPath $TransactionRoot -Force
}

function Remove-LAHJournaledTransaction {
    param([Parameter(Mandatory)][string]$TransactionRoot, [Parameter(Mandatory)]$Journal)
    if ([string]$Journal.operation -ceq 'uninstall') {
        Complete-LAHUninstallTransactionCleanup -TransactionRoot $TransactionRoot -Journal $Journal
    } else {
        Complete-LAHApplyTransactionCleanup -TransactionRoot $TransactionRoot
    }
}

function Add-LAHPlanMove {
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[object]]$Steps,
        [Parameter(Mandatory)][string]$Source,
        [Parameter(Mandatory)][string]$Destination,
        [Parameter(Mandatory)]$Record,
        [AllowNull()]$ExpectedDestination
    )
    if (-not (Test-LAHRecordAtPath -Path $Source -Record $Record)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    if (Test-Path -LiteralPath $Destination) {
        if ($null -eq $ExpectedDestination -or -not (Test-LAHRecordAtPath -Path $Destination -Record $ExpectedDestination)) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
    }
    $null = $Steps.Add([pscustomobject]@{ kind='move'; source=$Source; destination=$Destination; record=$Record; expectedDestination=$ExpectedDestination })
}

function Add-LAHPlanDelete {
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[object]]$Steps,
        [Parameter(Mandatory)][string]$Path,
        [AllowNull()]$Record
    )
    if (-not (Test-Path -LiteralPath $Path)) { return }
    if ($null -eq $Record) { $Record = Get-LAHFileRecord -Path $Path -Name ([IO.Path]::GetFileName($Path)) }
    if (-not (Test-LAHRecordAtPath -Path $Path -Record $Record)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    $null = $Steps.Add([pscustomobject]@{ kind='delete'; path=$Path; record=$Record })
}

function Add-LAHPlanDirectoryRemoval {
    param([Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[object]]$Steps, [Parameter(Mandatory)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) { return }
    Assert-LAHNoReparsePath -Path $Path
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    $null = $Steps.Add([pscustomobject]@{ kind='remove-directory'; path=$Path })
}

function Add-LAHPlanControlCleanup {
    param([Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[object]]$Steps, [Parameter(Mandatory)][string]$TransactionRoot)
    foreach ($name in @('journal.next','journal.previous','journal.json')) {
        $path = Join-Path $TransactionRoot $name
        if (Test-Path -LiteralPath $path -PathType Leaf) { Add-LAHPlanDelete -Steps $Steps -Path $path -Record $null }
    }
    Add-LAHPlanDirectoryRemoval -Steps $Steps -Path $TransactionRoot
}

function Add-LAHPlanApplyCleanup {
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[object]]$Steps,
        [Parameter(Mandatory)][string]$TransactionRoot,
        [string[]]$ExcludePaths = @()
    )
    $excluded = @{}
    foreach ($path in $ExcludePaths) { $excluded[[IO.Path]::GetFullPath($path)] = $true }
    foreach ($directoryName in @('new','backup')) {
        $directory = Join-Path $TransactionRoot $directoryName
        if (-not (Test-Path -LiteralPath $directory -PathType Container)) { continue }
        foreach ($file in @(Get-ChildItem -LiteralPath $directory -Force -File)) {
            if ($excluded.ContainsKey([IO.Path]::GetFullPath($file.FullName))) { continue }
            Add-LAHPlanDelete -Steps $Steps -Path $file.FullName -Record $null
        }
        Add-LAHPlanDirectoryRemoval -Steps $Steps -Path $directory
    }
    Add-LAHPlanControlCleanup -Steps $Steps -TransactionRoot $TransactionRoot
}

function Add-LAHPlanUninstallCleanup {
    param(
        [Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[object]]$Steps,
        [Parameter(Mandatory)][string]$TransactionRoot,
        [Parameter(Mandatory)]$Journal,
        [switch]$DeleteOwned
    )
    if ($DeleteOwned) {
        $oldMap = ConvertTo-LAHRecordMap -Records @($Journal.old_files) -AllowedNames $script:LAHInstallNames
        foreach ($name in $Journal.old_present) {
            Add-LAHPlanDelete -Steps $Steps -Path (Join-Path $TransactionRoot $name) -Record $oldMap[$name]
        }
        Add-LAHPlanDelete -Steps $Steps -Path (Join-Path $TransactionRoot $script:LAHReceiptName) -Record $Journal.old_receipt
    }
    Add-LAHPlanControlCleanup -Steps $Steps -TransactionRoot $TransactionRoot
}

function New-LAHRecoveryPlan {
    param([Parameter(Mandatory)][string]$InstallRoot, [Parameter(Mandatory)][string]$TransactionRoot, [Parameter(Mandatory)]$Journal)
    $steps = New-Object 'System.Collections.Generic.List[object]'
    $oldMap = ConvertTo-LAHRecordMap -Records @($Journal.old_files) -AllowedNames $script:LAHInstallNames
    $newMap = ConvertTo-LAHRecordMap -Records @($Journal.new_files) -AllowedNames $script:LAHInstallNames
    $oldPresent = @($Journal.old_present | ForEach-Object { [string]$_ })
    $rootReceipt = Join-Path $InstallRoot $script:LAHReceiptName
    $transactionReceipt = Join-Path $TransactionRoot $script:LAHReceiptName

    if ([string]$Journal.operation -ceq 'uninstall') {
        $rootHasReceipt = Test-Path -LiteralPath $rootReceipt
        $transactionHasReceipt = Test-Path -LiteralPath $transactionReceipt
        if ($rootHasReceipt) {
            if ([string]$Journal.phase -ceq 'committed' -or -not (Test-LAHRecordAtPath -Path $rootReceipt -Record $Journal.old_receipt) -or $transactionHasReceipt) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
            foreach ($name in $oldPresent) {
                $source = Join-Path $TransactionRoot $name
                $destination = Join-Path $InstallRoot $name
                if (Test-Path -LiteralPath $source) {
                    if (Test-Path -LiteralPath $destination) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
                    Add-LAHPlanMove -Steps $steps -Source $source -Destination $destination -Record $oldMap[$name] -ExpectedDestination $null
                } elseif (-not (Test-LAHRecordAtPath -Path $destination -Record $oldMap[$name])) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
            foreach ($name in $oldMap.Keys) {
                if (@($oldPresent) -cnotcontains $name -and (Test-Path -LiteralPath (Join-Path $InstallRoot $name))) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
            Add-LAHPlanControlCleanup -Steps $steps -TransactionRoot $TransactionRoot
            return [pscustomobject]@{ Steps=@($steps.ToArray()); UninstallCompleted=$false }
        }
        if ($transactionHasReceipt -and -not (Test-LAHRecordAtPath -Path $transactionReceipt -Record $Journal.old_receipt)) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        if (-not $transactionHasReceipt -and [string]$Journal.phase -cne 'committed') {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        if ([string]$Journal.phase -cne 'committed' -and -not $transactionHasReceipt) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        foreach ($name in $oldMap.Keys) {
            if (Test-Path -LiteralPath (Join-Path $InstallRoot $name)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        }
        if ([string]$Journal.phase -cne 'committed') {
            foreach ($name in $oldPresent) {
                if (-not (Test-LAHRecordAtPath -Path (Join-Path $TransactionRoot $name) -Record $oldMap[$name])) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
        } elseif (-not $transactionHasReceipt) {
            foreach ($name in $oldPresent) {
                if (Test-Path -LiteralPath (Join-Path $TransactionRoot $name)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
        }
        Add-LAHPlanUninstallCleanup -Steps $steps -TransactionRoot $TransactionRoot -Journal $Journal -DeleteOwned
        return [pscustomobject]@{ Steps=@($steps.ToArray()); UninstallCompleted=$true }
    }

    $rootHasNewReceipt = ($null -ne $Journal.new_receipt -and (Test-LAHRecordAtPath -Path $rootReceipt -Record $Journal.new_receipt))
    if ($rootHasNewReceipt) {
        foreach ($name in $newMap.Keys) {
            if (-not (Test-LAHRecordAtPath -Path (Join-Path $InstallRoot $name) -Record $newMap[$name])) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
        }
        Add-LAHPlanApplyCleanup -Steps $steps -TransactionRoot $TransactionRoot
        return [pscustomobject]@{ Steps=@($steps.ToArray()); UninstallCompleted=$false }
    }

    if ([string]$Journal.phase -ceq 'staging') {
        if ([string]$Journal.operation -ceq 'update' -or $null -ne $Journal.old_receipt) {
            if (-not (Test-LAHRecordAtPath -Path $rootReceipt -Record $Journal.old_receipt)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            foreach ($name in $oldPresent) {
                if (-not (Test-LAHRecordAtPath -Path (Join-Path $InstallRoot $name) -Record $oldMap[$name])) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            }
            foreach ($name in $newMap.Keys) {
                if (@($oldPresent) -cnotcontains $name -and (Test-Path -LiteralPath (Join-Path $InstallRoot $name))) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            }
        } else {
            if (Test-Path -LiteralPath $rootReceipt) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            foreach ($name in $newMap.Keys) { if (Test-Path -LiteralPath (Join-Path $InstallRoot $name)) { throw 'Lifecycle recovery is required; no unverified files were changed.' } }
        }
        Add-LAHPlanApplyCleanup -Steps $steps -TransactionRoot $TransactionRoot
        return [pscustomobject]@{ Steps=@($steps.ToArray()); UninstallCompleted=$false }
    }

    if ([string]$Journal.operation -ceq 'install' -and $null -eq $Journal.old_receipt) {
        if (Test-Path -LiteralPath $rootReceipt) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        $newRoot = Join-Path $TransactionRoot 'new'
        $moved = New-Object 'System.Collections.Generic.List[string]'
        foreach ($name in $newMap.Keys) {
            $destination = Join-Path $InstallRoot $name
            $source = Join-Path $newRoot $name
            if (Test-Path -LiteralPath $destination) {
                if (-not (Test-LAHRecordAtPath -Path $destination -Record $newMap[$name])) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            } else {
                Add-LAHPlanMove -Steps $steps -Source $source -Destination $destination -Record $newMap[$name] -ExpectedDestination $null
                $moved.Add($source)
            }
        }
        $stagedReceipt = Join-Path $newRoot $script:LAHReceiptName
        Add-LAHPlanMove -Steps $steps -Source $stagedReceipt -Destination $rootReceipt -Record $Journal.new_receipt -ExpectedDestination $null
        $moved.Add($stagedReceipt)
        Add-LAHPlanApplyCleanup -Steps $steps -TransactionRoot $TransactionRoot -ExcludePaths $moved.ToArray()
        return [pscustomobject]@{ Steps=@($steps.ToArray()); UninstallCompleted=$false }
    }

    if ([string]$Journal.operation -ceq 'update' -or ([string]$Journal.operation -ceq 'install' -and $null -ne $Journal.old_receipt)) {
        if ($null -eq $Journal.old_receipt) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        if (Test-Path -LiteralPath $rootReceipt) {
            if (-not (Test-LAHRecordAtPath -Path $rootReceipt -Record $Journal.old_receipt)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        }
        $backupRoot = Join-Path $TransactionRoot 'backup'
        $moved = New-Object 'System.Collections.Generic.List[string]'
        foreach ($name in $newMap.Keys) {
            if (@($oldPresent) -ccontains $name) { continue }
            $destination = Join-Path $InstallRoot $name
            if (Test-Path -LiteralPath $destination) {
                if (-not (Test-LAHRecordAtPath -Path $destination -Record $newMap[$name])) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
                Add-LAHPlanDelete -Steps $steps -Path $destination -Record $newMap[$name]
            }
        }
        foreach ($name in $oldPresent) {
            $destination = Join-Path $InstallRoot $name
            $backup = Join-Path $backupRoot $name
            $backupExists = Test-Path -LiteralPath $backup
            $destinationExists = Test-Path -LiteralPath $destination
            if ($backupExists) {
                if ($destinationExists) {
                    if (Test-LAHRecordAtPath -Path $destination -Record $oldMap[$name]) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
                    if (-not ($newMap.ContainsKey($name) -and (Test-LAHRecordAtPath -Path $destination -Record $newMap[$name]))) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
                    Add-LAHPlanDelete -Steps $steps -Path $destination -Record $newMap[$name]
                    Add-LAHPlanMove -Steps $steps -Source $backup -Destination $destination -Record $oldMap[$name] -ExpectedDestination $newMap[$name]
                } else {
                    Add-LAHPlanMove -Steps $steps -Source $backup -Destination $destination -Record $oldMap[$name] -ExpectedDestination $null
                }
                $moved.Add($backup)
            } elseif (-not (Test-LAHRecordAtPath -Path $destination -Record $oldMap[$name])) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
        }
        $backupReceipt = Join-Path $backupRoot $script:LAHReceiptName
        $backupReceiptExists = Test-Path -LiteralPath $backupReceipt
        $rootReceiptExists = Test-Path -LiteralPath $rootReceipt
        if ($backupReceiptExists) {
            if (-not (Test-LAHRecordAtPath -Path $backupReceipt -Record $Journal.old_receipt) -or $rootReceiptExists) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
            Add-LAHPlanMove -Steps $steps -Source $backupReceipt -Destination $rootReceipt -Record $Journal.old_receipt -ExpectedDestination $null
            $moved.Add($backupReceipt)
        } elseif (-not (Test-LAHRecordAtPath -Path $rootReceipt -Record $Journal.old_receipt)) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        Add-LAHPlanApplyCleanup -Steps $steps -TransactionRoot $TransactionRoot -ExcludePaths $moved.ToArray()
        return [pscustomobject]@{ Steps=@($steps.ToArray()); UninstallCompleted=$false }
    }
    throw 'Lifecycle recovery is required; no unverified files were changed.'
}

function Invoke-LAHApplyRecoveryPlan {
    param([Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Steps)
    foreach ($step in $Steps) {
        switch ([string]$step.kind) {
            'move' { [IO.File]::Move([string]$step.source, [string]$step.destination) }
            'delete' { Remove-Item -LiteralPath ([string]$step.path) -Force }
            'remove-directory' {
                if (@(Get-ChildItem -LiteralPath ([string]$step.path) -Force).Count -ne 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
                Remove-Item -LiteralPath ([string]$step.path) -Force
            }
            default { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        }
    }
}

function Read-LAHBoundedTextFile {
    param([Parameter(Mandatory)][string]$Path)
    $stream = $null
    try {
        $stream = [IO.FileStream]::new($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        if ($stream.Length -gt 1048576) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        $bytes = New-Object byte[] ([int]$stream.Length)
        $offset = 0
        while ($offset -lt $bytes.Length) {
            $read = $stream.Read($bytes, $offset, $bytes.Length - $offset)
            if ($read -le 0) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
            $offset += $read
        }
        return ([Text.UTF8Encoding]::new($false, $true)).GetString($bytes)
    } catch {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    } finally {
        if ($null -ne $stream) { $stream.Dispose() }
    }
}

function Assert-LAHReceiptMatchesMap {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)]$ReceiptRecord,
        [Parameter(Mandatory)][string]$InstallRoot,
        [Parameter(Mandatory)][hashtable]$ExpectedFiles
    )
    if (-not (Test-LAHRecordAtPath -Path $Path -Record $ReceiptRecord)) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    try { $receipt = Read-LAHBoundedTextFile -Path $Path | ConvertFrom-Json } catch {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    $expected = @('schema','product','install_root','files') | Sort-Object
    $actual = @($receipt.PSObject.Properties.Name | Sort-Object)
    if ($actual.Count -ne $expected.Count -or @((Compare-Object -ReferenceObject $expected -DifferenceObject $actual -CaseSensitive)).Count -ne 0 -or
        [string]$receipt.schema -cne 'lah-user-install-receipt-v1' -or [string]$receipt.product -cne $script:LAHProduct) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    try { $receiptRoot = Resolve-LAHPath -Path ([string]$receipt.install_root) -Label 'receipt install_root' } catch {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    if (-not [string]::Equals($receiptRoot, $InstallRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
    $actualFiles = ConvertTo-LAHRecordMap -Records @($receipt.files) -AllowedNames $script:LAHInstallNames
    if ($actualFiles.Count -ne $ExpectedFiles.Count) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    foreach ($name in $ExpectedFiles.Keys) {
        if (-not $actualFiles.ContainsKey($name) -or -not (Test-LAHNonNegativeJsonInteger -Value $actualFiles[$name].size) -or
            [long]$actualFiles[$name].size -ne [long]$ExpectedFiles[$name].size -or
            [string]$actualFiles[$name].sha256 -cne [string]$ExpectedFiles[$name].sha256) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
    }
}

function Assert-LAHRecoveryPlan {
    param(
        [Parameter(Mandatory)][string]$InstallRoot,
        [Parameter(Mandatory)][string]$TransactionRoot,
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Steps
    )
    $rootFull = [IO.Path]::GetFullPath($InstallRoot).TrimEnd('\') + '\'
    $moveSources = @{}
    $moveDestinations = @{}
    $deletePaths = @{}
    $removeDirectories = @{}
    foreach ($step in $Steps) {
        switch ([string]$step.kind) {
            'move' {
                $source = [IO.Path]::GetFullPath([string]$step.source)
                $destination = [IO.Path]::GetFullPath([string]$step.destination)
                if ((-not $source.StartsWith($rootFull, [StringComparison]::OrdinalIgnoreCase) -and
                     -not [string]::Equals($source.TrimEnd('\'), $InstallRoot.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)) -or
                    (-not $destination.StartsWith($rootFull, [StringComparison]::OrdinalIgnoreCase) -and
                     -not [string]::Equals($destination.TrimEnd('\'), $InstallRoot.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)) -or
                    $moveSources.ContainsKey($source) -or $moveDestinations.ContainsKey($destination)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
                $moveSources[$source] = $step
                $moveDestinations[$destination] = $step
            }
            'delete' {
                $path = [IO.Path]::GetFullPath([string]$step.path)
                if (-not $path.StartsWith($rootFull, [StringComparison]::OrdinalIgnoreCase) -or $deletePaths.ContainsKey($path)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
                $deletePaths[$path] = $step
            }
            'remove-directory' {
                $path = [IO.Path]::GetFullPath([string]$step.path)
                if (-not $path.StartsWith($rootFull, [StringComparison]::OrdinalIgnoreCase) -or $removeDirectories.ContainsKey($path)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
                $removeDirectories[$path] = $step
            }
            default { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        }
    }
    foreach ($path in $moveSources.Keys) {
        if ($deletePaths.ContainsKey($path)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    }
    foreach ($path in $moveDestinations.Keys) {
        if ($moveSources.ContainsKey($path)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
    }
    foreach ($step in $Steps) {
        switch ([string]$step.kind) {
            'move' {
                if (-not (Test-LAHRecordAtPath -Path ([string]$step.source) -Record $step.record)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
                $destination = [IO.Path]::GetFullPath([string]$step.destination)
                if (Test-Path -LiteralPath $destination -PathType Leaf) {
                    $allowedExisting = $false
                    if ($null -ne $step.expectedDestination -and
                        (Test-LAHRecordAtPath -Path $destination -Record $step.expectedDestination)) { $allowedExisting = $true }
                    if ($deletePaths.ContainsKey($destination) -and
                        (Test-LAHRecordAtPath -Path $destination -Record $deletePaths[$destination].record) -and
                        $null -ne $step.expectedDestination -and
                        [long]$deletePaths[$destination].record.size -eq [long]$step.expectedDestination.size -and
                        [string]$deletePaths[$destination].record.sha256 -ceq [string]$step.expectedDestination.sha256) { $allowedExisting = $true }
                    if (-not $allowedExisting) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
                } elseif ($null -ne $step.expectedDestination) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
            'delete' {
                if (-not (Test-LAHRecordAtPath -Path ([string]$step.path) -Record $step.record)) {
                    throw 'Lifecycle recovery is required; no unverified files were changed.'
                }
            }
            'remove-directory' {
                $path = [string]$step.path
                if (-not (Test-Path -LiteralPath $path -PathType Container)) { continue }
                Assert-LAHNoReparsePath -Path $path
                foreach ($child in @(Get-ChildItem -LiteralPath $path -Force)) {
                    $childPath = [IO.Path]::GetFullPath($child.FullName)
                    if ($child.PSIsContainer) {
                        if (-not $removeDirectories.ContainsKey($childPath)) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
                    } elseif (-not $moveSources.ContainsKey($childPath) -and -not $deletePaths.ContainsKey($childPath)) {
                        throw 'Lifecycle recovery is required; no unverified files were changed.'
                    }
                }
            }
        }
    }
}

function Invoke-LAHRecoverTransactions {
    param([Parameter(Mandatory)][string]$InstallRoot)
    try {
        Assert-LAHNoReparsePath -Path $InstallRoot
        $allRootEntries = @(Get-ChildItem -LiteralPath $InstallRoot -Force)
        $recognized = @($allRootEntries | Where-Object {
            $_.Name.StartsWith('.lah-transaction-', [StringComparison]::Ordinal) -or
            $_.Name.StartsWith('.lah-uninstall-', [StringComparison]::Ordinal)
        })
        if ($recognized.Count -gt 1) { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        if ($recognized.Count -eq 0) { return $false }

        $directory = $recognized[0]
        if (-not $directory.PSIsContainer -or
            $directory.Name -notmatch '^\.lah-(transaction|uninstall)-[0-9a-f]{32}$' -or
            (($directory.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        $transactionRoot = $directory.FullName
        Assert-LAHNoReparsePath -Path $transactionRoot
        $journal = Read-LAHTransactionJournal -TransactionRoot $transactionRoot -InstallRoot $InstallRoot
        if ($null -eq $journal) {
            if (@(Get-ChildItem -LiteralPath $transactionRoot -Force).Count -ne 0) {
                throw 'Lifecycle recovery is required; no unverified files were changed.'
            }
            $emptyPlan = @([pscustomobject]@{ kind='remove-directory'; path=$transactionRoot })
            Assert-LAHRecoveryPlan -InstallRoot $InstallRoot -TransactionRoot $transactionRoot -Steps $emptyPlan
            Invoke-LAHApplyRecoveryPlan -Steps $emptyPlan
            return $false
        }

        $isUninstall = [string]$journal.operation -ceq 'uninstall'
        if (($isUninstall -and -not $directory.Name.StartsWith('.lah-uninstall-', [StringComparison]::Ordinal)) -or
            (-not $isUninstall -and -not $directory.Name.StartsWith('.lah-transaction-', [StringComparison]::Ordinal))) {
            throw 'Lifecycle recovery is required; no unverified files were changed.'
        }
        Assert-LAHTransactionLayout -TransactionRoot $transactionRoot -Journal $journal
        $plan = New-LAHRecoveryPlan -InstallRoot $InstallRoot -TransactionRoot $transactionRoot -Journal $journal
        Assert-LAHRecoveryPlan -InstallRoot $InstallRoot -TransactionRoot $transactionRoot -Steps @($plan.Steps)
        Invoke-LAHApplyRecoveryPlan -Steps @($plan.Steps)
        return [bool]$plan.UninstallCompleted
    } catch {
        throw 'Lifecycle recovery is required; no unverified files were changed.'
    }
}

function Invoke-LAHApplyPackageCore {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$PackageRoot,
        [Parameter(Mandatory)][string]$InstallRoot,
        [Parameter(Mandatory)][ValidateSet('install','update')][string]$Operation
    )
    $package = Read-LAHPackage -PackageRoot $PackageRoot
    $root = Resolve-LAHPath -Path $InstallRoot -Label 'InstallRoot'
    Assert-LAHNoReparsePath -Path $root
    if (-not (Test-Path -LiteralPath $root -PathType Container)) { [IO.Directory]::CreateDirectory($root) | Out-Null }
    Assert-LAHNoReparsePath -Path $root

    $oldReceipt = Read-LAHInstallReceipt -InstallRoot $root -AllowMissingFiles
    if ($Operation -ceq 'update' -and $null -eq $oldReceipt) { throw 'No owned installation exists at the named root; use install first.' }
    if ($null -eq $oldReceipt) {
        foreach ($name in @($script:LAHInstallNames + $script:LAHReceiptName)) {
            if (Test-Path -LiteralPath (Join-Path $root $name)) { throw "The named install root contains a file not owned by this installer: $name" }
        }
    }
    $oldOwned = @{}
    if ($null -ne $oldReceipt) { foreach ($name in $oldReceipt.Files.Keys) { $oldOwned[$name] = $oldReceipt.Files[$name] } }
    foreach ($name in $script:LAHInstallNames) {
        if ((Test-Path -LiteralPath (Join-Path $root $name)) -and -not $oldOwned.ContainsKey($name)) {
            throw "Refusing to overwrite an unowned file in the named install root: $name"
        }
    }
    Assert-LAHNotRunning -InstallRoot $root

    $oldPresent = New-Object 'System.Collections.Generic.List[string]'
    $oldFiles = New-Object 'System.Collections.Generic.List[object]'
    foreach ($name in $oldOwned.Keys) {
        $oldFiles.Add($oldOwned[$name])
        $path = Join-Path $root $name
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            $record = Get-LAHFileRecord -Path $path -Name $name
            if ($record.size -ne [long]$oldOwned[$name].size -or $record.sha256 -cne [string]$oldOwned[$name].sha256) { throw 'An owned file changed before the lifecycle transaction began.' }
            $oldPresent.Add($name)
        }
    }
    $oldReceiptRecord = $null
    if ($null -ne $oldReceipt) { $oldReceiptRecord = Get-LAHFileRecord -Path $oldReceipt.Path -Name $script:LAHReceiptName }
    $newOwned = @{}
    foreach ($name in $oldOwned.Keys) { $newOwned[$name] = $oldOwned[$name] }
    foreach ($name in $script:LAHInstallNames) { $newOwned[$name] = $package.Files[$name] }
    $receiptText = New-LAHReceiptText -InstallRoot $root -Files $newOwned
    $receiptBytes = [Text.Encoding]::UTF8.GetBytes($receiptText)
    $hash = [Security.Cryptography.SHA256]::Create()
    try { $receiptHash = [BitConverter]::ToString($hash.ComputeHash($receiptBytes)).Replace('-','').ToLowerInvariant() } finally { $hash.Dispose() }
    $newReceiptRecord = [pscustomobject]@{ path=$script:LAHReceiptName; size=[long]$receiptBytes.Length; sha256=$receiptHash }

    $guid = [Guid]::NewGuid().ToString('N')
    $transactionName = ".lah-transaction-$guid"
    $transactionRoot = Join-Path $root $transactionName
    $newRoot = Join-Path $transactionRoot 'new'
    $backupRoot = Join-Path $transactionRoot 'backup'
    Assert-LAHNoReparsePath -Path $root
    [IO.Directory]::CreateDirectory($transactionRoot) | Out-Null
    [IO.Directory]::CreateDirectory($newRoot) | Out-Null
    [IO.Directory]::CreateDirectory($backupRoot) | Out-Null
    $newFileRecords = @($script:LAHInstallNames | ForEach-Object { $package.Files[$_] })
    $journal = [ordered]@{
        schema='lah-lifecycle-journal-v1'; operation=$Operation; phase='staging'; install_root=$root; transaction_name=$transactionName
        old_receipt=$oldReceiptRecord; old_files=@($oldFiles.ToArray()); old_present=@($oldPresent.ToArray())
        new_receipt=$newReceiptRecord; new_files=$newFileRecords
    }
    Write-LAHTransactionJournal -TransactionRoot $transactionRoot -Journal $journal
    try {
        foreach ($name in $script:LAHInstallNames) {
            $source = Join-Path $package.Root $name
            $stage = Join-Path $newRoot $name
            [IO.File]::Copy($source, $stage, $false)
            if (-not (Test-LAHRecordAtPath -Path $stage -Record $package.Files[$name])) { throw 'A package file changed while it was staged.' }
        }
        [IO.File]::WriteAllText((Join-Path $newRoot $script:LAHReceiptName), $receiptText, (New-Object Text.UTF8Encoding($false)))
        if (-not (Test-LAHRecordAtPath -Path (Join-Path $newRoot $script:LAHReceiptName) -Record $newReceiptRecord)) { throw 'The install receipt could not be staged safely.' }
        $journal.phase = 'ready'
        Write-LAHTransactionJournal -TransactionRoot $transactionRoot -Journal $journal

        if ($null -ne $oldReceipt) {
            [IO.File]::Move($oldReceipt.Path, (Join-Path $backupRoot $script:LAHReceiptName))
        }
        foreach ($name in $oldPresent) { [IO.File]::Move((Join-Path $root $name), (Join-Path $backupRoot $name)) }
        foreach ($name in ($script:LAHInstallNames | Sort-Object)) { [IO.File]::Move((Join-Path $newRoot $name), (Join-Path $root $name)) }
        [IO.File]::Move((Join-Path $newRoot $script:LAHReceiptName), (Join-Path $root $script:LAHReceiptName))
    } catch {
        $failure = $_
        try { $null = Invoke-LAHRecoverTransactions -InstallRoot $root } catch { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        throw $failure
    }
    $null = Invoke-LAHRecoverTransactions -InstallRoot $root
    Write-Output ("{0} completed for {1}" -f $Operation, $root)
}

function Invoke-LAHApplyPackage {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$PackageRoot,
        [Parameter(Mandatory)][string]$InstallRoot,
        [Parameter(Mandatory)][ValidateSet('install','update')][string]$Operation
    )
    $root = Resolve-LAHPath -Path $InstallRoot -Label 'InstallRoot'
    $mutex = Enter-LAHOperationMutex -InstallRoot $root
    $lease = $null
    try {
        $null = Read-LAHPackage -PackageRoot $PackageRoot
        Assert-LAHNoReparsePath -Path $root
        if (-not (Test-Path -LiteralPath $root -PathType Container)) { [IO.Directory]::CreateDirectory($root) | Out-Null }
        $lease = Open-LAHRootLease -InstallRoot $root
        $null = Invoke-LAHRecoverTransactions -InstallRoot $root
        Invoke-LAHApplyPackageCore -PackageRoot $PackageRoot -InstallRoot $root -Operation $Operation
    } finally {
        if ($null -ne $lease) { $lease.Dispose() }
        $mutex.ReleaseMutex()
        $mutex.Dispose()
    }
}

function Invoke-LAHUninstallCore {
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$InstallRoot)
    $root = Resolve-LAHPath -Path $InstallRoot -Label 'InstallRoot'
    if (-not (Test-Path -LiteralPath $root -PathType Container)) {
        Write-Output 'No installation exists at the named root.'
        return
    }
    Assert-LAHNoReparsePath -Path $root
    $receipt = Read-LAHInstallReceipt -InstallRoot $root -AllowMissingFiles
    if ($null -eq $receipt) { throw 'No valid install receipt exists; no files were changed.' }
    Assert-LAHNotRunning -InstallRoot $root
    $oldPresent = New-Object 'System.Collections.Generic.List[string]'
    $oldFiles = New-Object 'System.Collections.Generic.List[object]'
    foreach ($name in $receipt.Files.Keys) {
        $oldFiles.Add($receipt.Files[$name])
        $path = Join-Path $root $name
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            $record = Get-LAHFileRecord -Path $path -Name $name
            if ($record.size -ne [long]$receipt.Files[$name].size -or $record.sha256 -cne [string]$receipt.Files[$name].sha256) { throw 'An owned file changed before uninstall began; no files were changed.' }
            $oldPresent.Add($name)
        }
    }
    $oldReceiptRecord = Get-LAHFileRecord -Path $receipt.Path -Name $script:LAHReceiptName
    $guid = [Guid]::NewGuid().ToString('N')
    $transactionName = ".lah-uninstall-$guid"
    $transactionRoot = Join-Path $root $transactionName
    Assert-LAHNoReparsePath -Path $root
    [IO.Directory]::CreateDirectory($transactionRoot) | Out-Null
    $journal = [ordered]@{
        schema='lah-lifecycle-journal-v1'; operation='uninstall'; phase='ready'; install_root=$root; transaction_name=$transactionName
        old_receipt=$oldReceiptRecord; old_files=@($oldFiles.ToArray()); old_present=@($oldPresent.ToArray())
        new_receipt=$null; new_files=@()
    }
    Write-LAHTransactionJournal -TransactionRoot $transactionRoot -Journal $journal
    try {
        foreach ($name in $oldPresent) { [IO.File]::Move((Join-Path $root $name), (Join-Path $transactionRoot $name)) }
        [IO.File]::Move($receipt.Path, (Join-Path $transactionRoot $script:LAHReceiptName))
    } catch {
        $failure = $_
        try { $null = Invoke-LAHRecoverTransactions -InstallRoot $root } catch { throw 'Lifecycle recovery is required; no unverified files were changed.' }
        throw $failure
    }
    $journal.phase = 'committed'
    Write-LAHTransactionJournal -TransactionRoot $transactionRoot -Journal $journal
    Complete-LAHUninstallTransactionCleanup -TransactionRoot $transactionRoot -Journal $journal
    Write-Output ("Uninstall completed for {0}" -f $root)
}

function Invoke-LAHUninstall {
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$InstallRoot)
    $root = Resolve-LAHPath -Path $InstallRoot -Label 'InstallRoot'
    $mutex = Enter-LAHOperationMutex -InstallRoot $root
    $lease = $null
    try {
        if (Test-Path -LiteralPath $root -PathType Container) {
            $lease = Open-LAHRootLease -InstallRoot $root
            $recoveredUninstall = Invoke-LAHRecoverTransactions -InstallRoot $root
            if ($recoveredUninstall) {
                Write-Output ("Uninstall completed for {0}" -f $root)
                return
            }
        }
        Invoke-LAHUninstallCore -InstallRoot $root
    } finally {
        if ($null -ne $lease) { $lease.Dispose() }
        $mutex.ReleaseMutex()
        $mutex.Dispose()
    }
}

Export-ModuleMember -Function Get-LAHDefaultInstallRoot, Resolve-LAHDefaultInstallRootFrom, Invoke-LAHApplyPackage, Invoke-LAHUninstall
