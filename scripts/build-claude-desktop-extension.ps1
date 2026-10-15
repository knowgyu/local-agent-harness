[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$ExecutablePath,

    [ValidatePattern('^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$')]
    [string]$Version = '0.1.0-rc.6',

    [ValidatePattern('^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$')]
    [string]$McpbVersion = '2.1.2',

    [string]$OutputPath
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$repoOutputRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot 'output'))
$outputRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot 'output/claude-desktop'))
[IO.Directory]::CreateDirectory($repoOutputRoot) | Out-Null
[IO.Directory]::CreateDirectory($outputRoot) | Out-Null
foreach ($directoryPath in @($repoOutputRoot, $outputRoot)) {
    $directoryItem = Get-Item -LiteralPath $directoryPath
    if (($directoryItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'Package output directories must not be reparse points.'
    }
}
$repoOutputPrefix = $repoOutputRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar

if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $outputRoot "local-agent-harness-$Version-windows-amd64.mcpb"
} elseif (-not [IO.Path]::IsPathRooted($OutputPath)) {
    $OutputPath = Join-Path $repoRoot $OutputPath
}
$outputPath = [IO.Path]::GetFullPath($OutputPath)
if (-not $outputPath.StartsWith($repoOutputPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The MCPB output must remain under the ignored local output directory.'
}
if ([IO.Path]::GetExtension($outputPath) -ne '.mcpb') {
    throw 'The output filename must use the .mcpb extension.'
}
if (Test-Path -LiteralPath $outputPath) {
    throw 'The output file already exists; refusing to overwrite it.'
}

$sourcePath = if ([IO.Path]::IsPathRooted($ExecutablePath)) {
    [IO.Path]::GetFullPath($ExecutablePath)
} else {
    [IO.Path]::GetFullPath((Join-Path $repoRoot $ExecutablePath))
}
if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) {
    throw 'The explicitly supplied Windows executable does not exist.'
}
$source = Get-Item -LiteralPath $sourcePath
if ($source.Extension -ne '.exe' -or $source.Length -lt 2 -or ($source.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw 'The supplied file must be a regular, nonempty Windows .exe file.'
}

$fileStream = [IO.File]::OpenRead($sourcePath)
$binaryReader = [IO.BinaryReader]::new($fileStream)
try {
    if ($binaryReader.ReadUInt16() -ne 0x5A4D -or $fileStream.Length -lt 0x40) {
        throw 'The supplied file does not have a Windows executable header.'
    }
    $fileStream.Position = 0x3C
    $peOffset = $binaryReader.ReadInt32()
    if ($peOffset -lt 0x40 -or ($peOffset + 6) -gt $fileStream.Length) {
        throw 'The supplied file has an invalid Windows PE header offset.'
    }
    $fileStream.Position = $peOffset
    if ($binaryReader.ReadUInt32() -ne 0x00004550 -or $binaryReader.ReadUInt16() -ne 0x8664) {
        throw 'The supplied file must be a Windows amd64 PE executable.'
    }
} finally {
    $binaryReader.Dispose()
    $fileStream.Dispose()
}

$go = Get-Command go -ErrorAction Stop
$goBuildInfo = (& $go.Source version -m $sourcePath 2>$null | Out-String)
if ($LASTEXITCODE -ne 0 -or $goBuildInfo -notmatch '(?m)^\s*build\s+-trimpath=true\s*$' -or
    $goBuildInfo -notmatch '(?m)^\s*build\s+GOOS=windows\s*$' -or
    $goBuildInfo -notmatch '(?m)^\s*build\s+GOARCH=amd64\s*$' -or
    $goBuildInfo -match '(?m)^\s*build\s+vcs\.(revision|modified)=') {
    throw 'The supplied executable is not a privacy-trimmed Windows amd64 Go build. Use the final Windows amd64 release build.'
}

function Get-Sha256([string]$Path) {
    (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Test-BinaryPrivacy([string]$Path) {
    $markers = @()
    if (-not [string]::IsNullOrWhiteSpace($env:USERNAME) -and $env:USERNAME.Length -ge 3) {
        $markers += $env:USERNAME
    }
    if (-not [string]::IsNullOrWhiteSpace($env:USERPROFILE)) {
        $profileName = Split-Path -Leaf $env:USERPROFILE
        if ($profileName.Length -ge 3) { $markers += $profileName }
    }
    $markers = @($markers | Sort-Object -Unique)
    $bytes = [IO.File]::ReadAllBytes($Path)
    $textForms = @(
        [Text.Encoding]::ASCII.GetString($bytes),
        [Text.Encoding]::Unicode.GetString($bytes),
        [Text.Encoding]::BigEndianUnicode.GetString($bytes)
    )
    $emailPattern = '(?i)[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}'
    $profilePathPattern = '(?i)[A-Z]:[\\/]+Users[\\/]+[^\\/\x00\r\n\s]+'
    foreach ($text in $textForms) {
        foreach ($marker in $markers) {
            if ($text.IndexOf($marker, [StringComparison]::OrdinalIgnoreCase) -ge 0) {
                throw 'The executable contains the current Windows account or profile name.'
            }
        }
        if ([regex]::IsMatch($text, $emailPattern) -or [regex]::IsMatch($text, $profilePathPattern)) {
            throw 'The executable contains an email address or a Windows user profile path.'
        }
    }
}

Test-BinaryPrivacy $sourcePath

$mcpb = Get-Command mcpb -ErrorAction SilentlyContinue
$npx = $null
if ($null -eq $mcpb) {
    $npx = Get-Command npx -ErrorAction SilentlyContinue
    if ($null -eq $npx) {
        throw "MCPB CLI not found. Install Node.js or install the official CLI with: npm install -g @anthropic-ai/mcpb@$McpbVersion"
    }
}

function Invoke-Mcpb([string[]]$Arguments) {
    if ($null -ne $mcpb) {
        & $mcpb.Source @Arguments
    } else {
        $npxArguments = @('--yes', '--package', "@anthropic-ai/mcpb@$McpbVersion", 'mcpb') + $Arguments
        & $npx.Source @npxArguments
    }
    if ($LASTEXITCODE -ne 0) {
        throw 'The pinned official MCPB CLI command failed.'
    }
}

$installedMcpbVersion = (Invoke-Mcpb -Arguments @('--version') | Out-String).Trim()
if ($installedMcpbVersion -ne $McpbVersion) {
    throw "MCPB CLI version $installedMcpbVersion is installed; this build expects version $McpbVersion."
}

$stageRoot = Join-Path $outputRoot ('.stage-' + [guid]::NewGuid().ToString('N'))
$stagedArchivePath = Join-Path $outputRoot ('.stage-' + [guid]::NewGuid().ToString('N') + '.mcpb')
$stageRootFull = [IO.Path]::GetFullPath($stageRoot)
$stagedArchivePathFull = [IO.Path]::GetFullPath($stagedArchivePath)
$outputRootPrefix = $outputRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
if (-not $stageRootFull.StartsWith($outputRootPrefix, [StringComparison]::OrdinalIgnoreCase) -or
    -not $stagedArchivePathFull.StartsWith($outputRootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The temporary package paths must remain within the local output directory.'
}

try {
    [IO.Directory]::CreateDirectory((Join-Path $stageRoot 'server')) | Out-Null
    $manifestSource = Join-Path $repoRoot 'packaging/claude-desktop/manifest.json'
    if (-not (Test-Path -LiteralPath $manifestSource -PathType Leaf)) {
        throw 'The Claude Desktop manifest template is missing.'
    }

    $manifest = Get-Content -LiteralPath $manifestSource -Raw | ConvertFrom-Json
    if ($manifest.manifest_version -ne '0.3' -or $manifest.server.type -ne 'binary' -or
        $manifest.server.entry_point -ne 'server/local-agent-harness' -or
        $manifest.server.mcp_config.command -ne 'server/local-agent-harness' -or
        @($manifest.server.mcp_config.args).Count -ne 1 -or $manifest.server.mcp_config.args[0] -ne '--mcp') {
        throw 'The manifest template no longer describes the expected Windows stdio entry point.'
    }
    if ($manifest.PSObject.Properties.Name -contains 'user_config') {
        throw 'The package must not embed or request a private Local Agent Harness settings path or secret.'
    }
    $manifest.version = $Version
    $manifestPath = Join-Path $stageRoot 'manifest.json'
    [IO.File]::WriteAllText($manifestPath, (($manifest | ConvertTo-Json -Depth 30) + "`n"), [Text.UTF8Encoding]::new($false))
    Copy-Item -LiteralPath $sourcePath -Destination (Join-Path $stageRoot 'server/local-agent-harness.exe')

    Invoke-Mcpb -Arguments @('validate', $stageRoot)
    Invoke-Mcpb -Arguments @('pack', $stageRoot, $stagedArchivePath)
    if (-not (Test-Path -LiteralPath $stagedArchivePathFull -PathType Leaf)) {
        throw 'The official MCPB CLI could not create the package.'
    }
    Invoke-Mcpb -Arguments @('info', $stagedArchivePathFull)

    $archive = [IO.Compression.ZipFile]::OpenRead($stagedArchivePathFull)
    try {
        $expectedEntries = @('manifest.json', 'server/local-agent-harness.exe')
        $actualEntries = @($archive.Entries | ForEach-Object { $_.FullName } | Sort-Object -CaseSensitive)
        $expectedSorted = @($expectedEntries | Sort-Object -CaseSensitive)
        if ($actualEntries.Count -ne $expectedSorted.Count) {
            throw 'The package contains unexpected files.'
        }
        for ($index = 0; $index -lt $expectedSorted.Count; $index++) {
            if ($actualEntries[$index] -cne $expectedSorted[$index]) {
                throw 'The package contains unexpected files.'
            }
        }

        $manifestEntry = $archive.GetEntry('manifest.json')
        $manifestReader = [IO.StreamReader]::new($manifestEntry.Open(), [Text.Encoding]::UTF8)
        try { $packedManifest = $manifestReader.ReadToEnd() | ConvertFrom-Json } finally { $manifestReader.Dispose() }
        if ($packedManifest.version -ne $Version -or $packedManifest.server.mcp_config.command -ne 'server/local-agent-harness' -or
            @($packedManifest.server.mcp_config.args).Count -ne 1 -or $packedManifest.server.mcp_config.args[0] -ne '--mcp') {
            throw 'The packed manifest does not match the requested version and stdio command.'
        }

        $executableEntry = $archive.GetEntry('server/local-agent-harness.exe')
        $entryStream = $executableEntry.Open()
        $sha = [Security.Cryptography.SHA256]::Create()
        try { $packedExecutableHash = [BitConverter]::ToString($sha.ComputeHash($entryStream)).Replace('-', '').ToLowerInvariant() } finally { $sha.Dispose(); $entryStream.Dispose() }
        if ($packedExecutableHash -ne (Get-Sha256 $sourcePath)) {
            throw 'The packaged executable does not match the explicitly supplied executable.'
        }
    } finally {
        $archive.Dispose()
    }

    Move-Item -LiteralPath $stagedArchivePathFull -Destination $outputPath
    [pscustomobject]@{
        Package = $outputPath
        Version = $Version
        McpbCliVersion = $McpbVersion
        ExecutableSha256 = Get-Sha256 $sourcePath
        PackageSha256 = Get-Sha256 $outputPath
        Contents = 'manifest.json; server/local-agent-harness.exe'
        Signature = 'unsigned'
    }
} finally {
    if (Test-Path -LiteralPath $stagedArchivePathFull -PathType Leaf) {
        Remove-Item -LiteralPath $stagedArchivePathFull -Force
    }
    if (Test-Path -LiteralPath $stageRootFull -PathType Container) {
        $stageItem = Get-Item -LiteralPath $stageRootFull
        if (($stageItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
            -not $stageRootFull.StartsWith($outputRootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'Refusing to remove an unexpected package staging path.'
        }
        Remove-Item -LiteralPath $stageRootFull -Recurse -Force
    }
}
