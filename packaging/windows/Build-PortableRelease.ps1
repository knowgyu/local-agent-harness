[CmdletBinding()]
param(
    [ValidatePattern('^v0\.1\.0-rc\.2$')]
    [string]$Version = 'v0.1.0-rc.2',
    [string]$OutputRoot = 'output/privacy-release/package'
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$privacyRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot 'output/privacy-release'))
$outputPath = if ([IO.Path]::IsPathRooted($OutputRoot)) {
    [IO.Path]::GetFullPath($OutputRoot)
} else {
    [IO.Path]::GetFullPath((Join-Path $repoRoot $OutputRoot))
}
$privacyPrefix = $privacyRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
if (-not $outputPath.StartsWith($privacyPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Package output must remain under the local privacy-release output directory.'
}
$outputRelativePath = $outputPath.Substring($repoRoot.Length).TrimStart([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar).Replace([char]92, '/')
if (Test-Path -LiteralPath $outputPath) {
    throw 'Package output already exists; refusing to overwrite it.'
}

$versionSuffix = $Version
$packageName = "local-agent-harness-$versionSuffix-windows-amd64.zip"
$checksumName = "local-agent-harness-$versionSuffix-windows-amd64.sha256"
$receiptName = "local-agent-harness-$versionSuffix-windows-amd64.package-audit.json"
$payloadNames = @(
    'local-agent-harness.exe',
    'README.md',
    'CLIENT_SETUP.md',
    'Install-LocalAgentHarness.ps1',
    'Update-LocalAgentHarness.ps1',
    'Uninstall-LocalAgentHarness.ps1',
    'LocalAgentHarness.Lifecycle.psm1'
)
$lifecycleNames = @(
    'Install-LocalAgentHarness.ps1',
    'Update-LocalAgentHarness.ps1',
    'Uninstall-LocalAgentHarness.ps1',
    'LocalAgentHarness.Lifecycle.psm1'
)
$stageRoot = Join-Path $privacyRoot ('.lah-package-stage-' + [guid]::NewGuid().ToString('N'))
$packageRoot = Join-Path $stageRoot 'package-root'
$buildRoot = Join-Path $stageRoot 'build'
[IO.Directory]::CreateDirectory($packageRoot) | Out-Null
[IO.Directory]::CreateDirectory($buildRoot) | Out-Null

function Get-FileSha256([string]$Path) {
    (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function ConvertTo-HexString([byte[]]$Bytes) {
    [BitConverter]::ToString($Bytes).Replace('-', '').ToLowerInvariant()
}

function Get-RelativeSourcePath([string]$Path) {
    $fullPath = [IO.Path]::GetFullPath($Path)
    $rootPrefix = $repoRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    if (-not $fullPath.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'A runtime input resolved outside the source root.'
    }
    $fullPath.Substring($rootPrefix.Length).Replace([char]92, '/')
}

function Get-RuntimeInputRecords([string]$GoPath) {
    $template = '{{range .GoFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .CgoFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .CFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .CXXFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .HFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .FFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .SFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .SysoFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}{{range .EmbedFiles}}{{printf "%s/%s\n" $.Dir .}}{{end}}'
    Push-Location $repoRoot
    try {
        $listedFiles = @(& $GoPath list -f $template ./... 2>$null)
        if ($LASTEXITCODE -ne 0) { throw 'Go could not enumerate Windows runtime inputs.' }
    } finally {
        Pop-Location
    }

    $paths = [System.Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($path in @('go.mod', 'go.sum')) {
        $fullPath = Join-Path $repoRoot $path
        if (-not (Test-Path -LiteralPath $fullPath -PathType Leaf)) {
            if ($path -eq 'go.mod') { throw 'The Go module file is missing.' }
            continue
        }
        [void]$paths.Add($fullPath)
    }
    foreach ($listed in $listedFiles) {
        $fullPath = [string]$listed
        if ([string]::IsNullOrWhiteSpace($fullPath)) { continue }
        $fullPath = [IO.Path]::GetFullPath($fullPath.Trim())
        if (-not (Test-Path -LiteralPath $fullPath -PathType Leaf)) {
            throw 'Go listed a missing Windows runtime input.'
        }
        [void]$paths.Add($fullPath)
    }
    foreach ($name in $lifecycleNames) {
        $fullPath = Join-Path $repoRoot (Join-Path 'packaging/windows' $name)
        if (-not (Test-Path -LiteralPath $fullPath -PathType Leaf)) {
            throw 'A canonical lifecycle source file is missing.'
        }
        [void]$paths.Add($fullPath)
    }

    $records = foreach ($fullPath in $paths) {
        [ordered]@{
            path = Get-RelativeSourcePath $fullPath
            size = [int64](Get-Item -LiteralPath $fullPath).Length
            sha256 = Get-FileSha256 $fullPath
        }
    }
    $recordList = [System.Collections.Generic.List[object]]::new()
    foreach ($record in @($records)) { $recordList.Add($record) }
    $recordList.Sort([Comparison[object]]{
        param($left, $right)
        [StringComparer]::Ordinal.Compare([string]$left.path, [string]$right.path)
    })
    return ,@($recordList.ToArray())
}

function Get-ContentIdentitySha256([object[]]$Records) {
    $builder = [Text.StringBuilder]::new()
    foreach ($record in $Records) {
        [void]$builder.Append([string]$record.path).Append([char]0)
        [void]$builder.Append([string]$record.size).Append([char]0)
        [void]$builder.Append([string]$record.sha256).Append([char]10)
    }
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($builder.ToString())
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        ConvertTo-HexString $sha.ComputeHash($bytes)
    } finally {
        $sha.Dispose()
    }
}

function Write-Utf8NoBom([string]$Path, [string]$Text) {
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Test-PackagePrivacy([string[]]$Paths) {
    $accountMarkers = @()
    if (-not [string]::IsNullOrWhiteSpace($env:USERNAME) -and $env:USERNAME.Length -ge 3) {
        $accountMarkers += $env:USERNAME
    }
    if (-not [string]::IsNullOrWhiteSpace($env:USERPROFILE)) {
        $profileName = Split-Path -Leaf $env:USERPROFILE
        if ($profileName.Length -ge 3) { $accountMarkers += $profileName }
    }
    $accountMarkers = @($accountMarkers | Sort-Object -Unique)
    $emailPattern = '(?i)[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}'
    $profilePathPattern = '(?i)[A-Z]:[\\/]+Users[\\/]+[^\\/\x00\r\n\s]+'

    foreach ($path in $Paths) {
        $bytes = [IO.File]::ReadAllBytes($path)
        $ascii = [Text.Encoding]::ASCII.GetString($bytes)
        $utf16le = [Text.Encoding]::Unicode.GetString($bytes)
        $utf16be = [Text.Encoding]::BigEndianUnicode.GetString($bytes)
        foreach ($text in @($ascii, $utf16le, $utf16be)) {
            foreach ($marker in $accountMarkers) {
                if ($text.IndexOf($marker, [StringComparison]::OrdinalIgnoreCase) -ge 0) {
                    throw 'A package file contains a local account identifier.'
                }
            }
            if ([regex]::IsMatch($text, $emailPattern)) {
                throw 'A package file contains an email address.'
            }
            if ([regex]::IsMatch($text, $profilePathPattern)) {
                throw 'A package file contains a Windows user profile path.'
            }
        }
    }
}

function Assert-ExactZip([string]$ZipPath, [object[]]$Records) {
    $archive = [IO.Compression.ZipFile]::OpenRead($ZipPath)
    try {
        if ($archive.Entries.Count -ne $Records.Count) {
            throw 'The release ZIP does not contain the exact expected member count.'
        }
        $expected = @{}
        foreach ($record in $Records) { $expected[[string]$record.path] = $record }
        $seen = @{}
        foreach ($entry in $archive.Entries) {
            $name = [string]$entry.FullName
            if (-not $expected.ContainsKey($name) -or $seen.ContainsKey($name)) {
                throw 'The release ZIP contains an unexpected or repeated member.'
            }
            $seen[$name] = $true
            if ($entry.Length -ne [int64]$expected[$name].size) {
                throw 'A release ZIP member has an unexpected size.'
            }
            $stream = $entry.Open()
            try {
                $sha = [Security.Cryptography.SHA256]::Create()
                try { $actual = ConvertTo-HexString $sha.ComputeHash($stream) } finally { $sha.Dispose() }
            } finally {
                $stream.Dispose()
            }
            if ($actual -ne [string]$expected[$name].sha256) {
                throw 'A release ZIP member has an unexpected SHA-256.'
            }
        }
    } finally {
        $archive.Dispose()
    }
}

$go = Get-Command go -ErrorAction Stop
$goVersion = (& $go.Source version 2>$null | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $goVersion -notmatch '^go version go[0-9.]+ windows/amd64$') {
    throw 'A native Windows amd64 Go toolchain is required.'
}
$goSumPath = Join-Path $repoRoot 'go.sum'
$goSumBefore = if (Test-Path -LiteralPath $goSumPath -PathType Leaf) { Get-FileSha256 $goSumPath } else { $null }
$runtimeInputRecords = Get-RuntimeInputRecords $go.Source
$runtimeInputIdentity = Get-ContentIdentitySha256 $runtimeInputRecords
$buildPaths = @(
    (Join-Path $buildRoot 'local-agent-harness-build-one.exe'),
    (Join-Path $buildRoot 'local-agent-harness-build-two.exe')
)

for ($index = 0; $index -lt $buildPaths.Count; $index++) {
    Push-Location $repoRoot
    try {
        & $go.Source build -mod=readonly -trimpath -buildvcs=false -o $buildPaths[$index] . 2>$null
        if ($LASTEXITCODE -ne 0) { throw 'A native Windows amd64 release build failed.' }
    } finally {
        Pop-Location
    }
}
$binaryHash = Get-FileSha256 $buildPaths[0]
if ($binaryHash -ne (Get-FileSha256 $buildPaths[1])) {
    throw 'Two native builds produced different executable bytes.'
}
if ($goSumBefore -ne $(if (Test-Path -LiteralPath $goSumPath -PathType Leaf) { Get-FileSha256 $goSumPath } else { $null })) {
    throw 'The package build changed go.sum.'
}

$buildInfo = @(& $go.Source version -m $buildPaths[0] 2>$null)
if ($LASTEXITCODE -ne 0) { throw 'Go could not inspect the release executable metadata.' }
$buildInfoText = $buildInfo -join "`n"
if ($buildInfoText -notmatch '(?m)^\s*build\s+-trimpath=true\s*$' -or
    $buildInfoText -match '(?m)^\s*build\s+vcs\.(revision|modified)=') {
    throw 'The executable metadata does not meet the release privacy build settings.'
}

Copy-Item -LiteralPath $buildPaths[0] -Destination (Join-Path $packageRoot 'local-agent-harness.exe')
Copy-Item -LiteralPath (Join-Path $repoRoot 'docs/CLIENT_SETUP.md') -Destination (Join-Path $packageRoot 'CLIENT_SETUP.md')
foreach ($name in $lifecycleNames) {
    Copy-Item -LiteralPath (Join-Path $repoRoot (Join-Path 'packaging/windows' $name)) -Destination (Join-Path $packageRoot $name)
}
$readme = @(
    '# Local Agent Harness - Windows portable package',
    '',
    "버전: $Version",
    '',
    '검증한 ZIP을 통째로 압축 해제한 뒤 포함된 설치 스크립트를 실행하세요.',
    '',
    '현재 Windows 사용자에게 설치하려면 이 패키지 폴더에서 실행하세요.',
    'powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install-LocalAgentHarness.ps1',
    '',
    '기본 설치 위치는 %LOCALAPPDATA%\Programs\Local Agent Harness입니다. -InstallRoot는 사용자가 직접 정한 다른 설치 위치를 지정할 때 사용하세요. 업데이트할 때는 새 검증 패키지를 압축 해제하고 해당 폴더에서 Update-LocalAgentHarness.ps1을 실행하세요. 제거할 때는 설치 폴더나 패키지 복사본에서 Uninstall-LocalAgentHarness.ps1을 실행하세요. 기본 위치와 다르면 -InstallRoot를 전달해야 합니다. 제거 스크립트는 영수증에 기록된 파일 중 내용이 바뀌지 않은 항목만 지우고, 알 수 없는 파일과 사용자 데이터는 남깁니다.',
    '업데이트가 실패하면 패키지가 관리하는 파일 변경을 되돌립니다. 스크립트는 앱이나 자식 프로세스를 종료하지 않으며, 실행 중인 앱이 있으면 업데이트를 거부합니다.',
    '제거 도중 마지막 파일 삭제가 실패하면 일부 항목이 남아 제거가 끝나지 않을 수 있습니다. 이 경우 알 수 없는 파일과 설치 폴더는 그대로 남습니다.',
    '설치·업데이트·제거의 정해진 중단 지점에서 프로세스 종료 후 복구를 확인했습니다. 갑작스러운 전원 차단 뒤의 복구까지 확인한 것은 아닙니다.',
    '스크립트를 다시 실행하면 요청한 작업 전에 검증된 저널을 복구합니다. 복구 데이터가 잘못되었거나 설치 상태와 맞지 않으면 고정된 오류를 반환하고 확인되지 않은 파일을 보존합니다. 트랜잭션 데이터를 직접 지우지 마세요.',
    'JavaScript를 끄면 대화형 설정과 일부 실시간 상태 기능이 제한됩니다. 정적 /readiness 안내를 참고하세요. 홈 화면에는 영어가 일부 남아 있으며 전체 서버 렌더링 동작은 아직 확인하지 않았습니다.',
    '앱을 종료해도 저장된 gateway token이나 원격 자격 증명은 폐기되지 않습니다.',
    '',
    '설치 스크립트는 관리자 권한을 요구하지 않으며 시작 항목, Run 키, 클라이언트 CLI 설정, 영구 환경변수, Credential Manager를 변경하지 않습니다. SHA-256은 파일 손상을 확인하는 값이며 배포자를 인증하지 않습니다. 이 패키지는 서명되지 않았습니다.',
    '',
    '수동 클라이언트 설정과 확인된 로컬 인터페이스는 CLIENT_SETUP.md를 참고하세요.'
) -join "`n"
Write-Utf8NoBom (Join-Path $packageRoot 'README.md') ($readme + "`n")

$privacyScanPaths = [System.Collections.Generic.List[string]]::new()
foreach ($file in Get-ChildItem -LiteralPath $packageRoot -File) { $privacyScanPaths.Add($file.FullName) }
foreach ($record in $runtimeInputRecords) {
    $privacyScanPaths.Add((Join-Path $repoRoot ([string]$record.path).Replace('/', [IO.Path]::DirectorySeparatorChar)))
}
Test-PackagePrivacy $privacyScanPaths.ToArray()

$payloadFiles = foreach ($name in $payloadNames) {
    $path = Join-Path $packageRoot $name
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw 'A required release payload file is missing.' }
    [ordered]@{ path = $name; size = [int64](Get-Item -LiteralPath $path).Length; sha256 = Get-FileSha256 $path }
}
$manifest = [ordered]@{
    schema = 'lah-portable-package-manifest-v1'
    product = 'local-agent-harness'
    display_title = 'Local Agent Harness'
    package_kind = 'portable-local-package'
    package_version = $Version
    source = [ordered]@{
        identity_schema = 'lah-runtime-input-content-sha256-v1'
        runtime_inputs_sha256 = $runtimeInputIdentity
        runtime_inputs = @($runtimeInputRecords)
    }
    build = [ordered]@{
        go_version = $goVersion.Split(' ')[2]
        goos = 'windows'
        goarch = 'amd64'
        flags = @('-mod=readonly', '-trimpath', '-buildvcs=false')
        reproducible_binary = $true
        executable_sha256 = $binaryHash
        validation = @('native Windows amd64 build pass 1', 'native Windows amd64 build pass 2', 'executable build metadata privacy check')
    }
    files = @($payloadFiles)
    lifecycle_source = [ordered]@{
        source_root = 'packaging/windows'
        files = @($lifecycleNames | ForEach-Object {
            $lifecycleName = $_
            $sourcePath = "packaging/windows/$lifecycleName"
            $record = $runtimeInputRecords | Where-Object { $_.path -ceq $sourcePath } | Select-Object -First 1
            if ($null -eq $record) { throw 'A lifecycle input is missing from the runtime identity.' }
            [ordered]@{ source_path = $record.path; package_path = $lifecycleName; size = $record.size; sha256 = $record.sha256 }
        })
    }
}
Write-Utf8NoBom (Join-Path $packageRoot 'manifest.json') (($manifest | ConvertTo-Json -Depth 20) + "`n")

$sumNames = @($payloadNames + 'manifest.json')
$sumNames = @($sumNames | Sort-Object -CaseSensitive)
$sumLines = foreach ($name in $sumNames) { "$(Get-FileSha256 (Join-Path $packageRoot $name))  $name" }
Write-Utf8NoBom (Join-Path $packageRoot 'SHA256SUMS.txt') ([string]::Join("`n", $sumLines) + "`n")

$zipPath = Join-Path $stageRoot $packageName
$zipStream = [IO.File]::Open($zipPath, [IO.FileMode]::CreateNew)
try {
    $archive = [IO.Compression.ZipArchive]::new($zipStream, [IO.Compression.ZipArchiveMode]::Create, $false)
    try {
        $zipNames = @((Get-ChildItem -LiteralPath $packageRoot -File | ForEach-Object { $_.Name }) | Sort-Object -CaseSensitive)
        foreach ($name in $zipNames) {
            $entry = $archive.CreateEntry($name, [IO.Compression.CompressionLevel]::Optimal)
            $entry.LastWriteTime = [DateTimeOffset]::new(1980, 1, 1, 0, 0, 0, [TimeSpan]::Zero)
            $input = [IO.File]::OpenRead((Join-Path $packageRoot $name))
            try {
                $output = $entry.Open()
                try { $input.CopyTo($output) } finally { $output.Dispose() }
            } finally {
                $input.Dispose()
            }
        }
    } finally {
        $archive.Dispose()
    }
} finally {
    $zipStream.Dispose()
}

$packageMembers = foreach ($name in $zipNames) {
    $path = Join-Path $packageRoot $name
    [ordered]@{ path = $name; size = [int64](Get-Item -LiteralPath $path).Length; sha256 = Get-FileSha256 $path }
}
Assert-ExactZip $zipPath @($packageMembers)
foreach ($file in Get-ChildItem -LiteralPath $packageRoot -File) {
    if (-not $privacyScanPaths.Contains($file.FullName)) { $privacyScanPaths.Add($file.FullName) }
}
Test-PackagePrivacy $privacyScanPaths.ToArray()

$zipHash = Get-FileSha256 $zipPath
$checksumPath = Join-Path $stageRoot $checksumName
Write-Utf8NoBom $checksumPath "$zipHash  $packageName`n"
$receipt = [ordered]@{
    schema = 'lah-portable-package-build-audit-v1'
    package_version = $Version
    package_zip = $packageName
    package_zip_sha256 = $zipHash
    package_member_count = $packageMembers.Count
    package_members = @($packageMembers)
    runtime_inputs_sha256 = $runtimeInputIdentity
    executable_sha256 = $binaryHash
    build = [ordered]@{
        go_version = $goVersion.Split(' ')[2]
        target = 'windows/amd64'
        flags = @('-mod=readonly', '-trimpath', '-buildvcs=false')
        reproducible_binary = $true
    }
    privacy_scan = [ordered]@{
        username_marker = 'absent'
        user_profile_path = 'absent'
        email_address = 'absent'
        go_vcs_revision = 'absent'
    }
}
$receiptPath = Join-Path $stageRoot $receiptName
Write-Utf8NoBom $receiptPath (($receipt | ConvertTo-Json -Depth 20) + "`n")

$stagedPackageFiles = @(Get-ChildItem -LiteralPath $packageRoot -File)
$stagedPackageDirectories = @(Get-ChildItem -LiteralPath $packageRoot -Directory)
if ($stagedPackageDirectories.Count -ne 0 -or $stagedPackageFiles.Count -ne $zipNames.Count -or
    @($stagedPackageFiles | Where-Object { $zipNames -cnotcontains $_.Name }).Count -ne 0) {
    throw 'The verified package staging directory contains unexpected files.'
}
foreach ($file in $stagedPackageFiles) { [IO.File]::Delete($file.FullName) }
[IO.Directory]::Delete($packageRoot)

$stagedBuildFiles = @(Get-ChildItem -LiteralPath $buildRoot -File)
$expectedBuildNames = @('local-agent-harness-build-one.exe', 'local-agent-harness-build-two.exe')
$stagedBuildDirectories = @(Get-ChildItem -LiteralPath $buildRoot -Directory)
if ($stagedBuildDirectories.Count -ne 0 -or $stagedBuildFiles.Count -ne $expectedBuildNames.Count -or
    @($stagedBuildFiles | Where-Object { $expectedBuildNames -cnotcontains $_.Name }).Count -ne 0) {
    throw 'The verified build staging directory contains unexpected files.'
}
foreach ($file in $stagedBuildFiles) { [IO.File]::Delete($file.FullName) }
[IO.Directory]::Delete($buildRoot)

[IO.Directory]::CreateDirectory((Split-Path -Parent $outputPath)) | Out-Null
if (Test-Path -LiteralPath $outputPath) { throw 'Package output appeared during the build; refusing to overwrite it.' }
[IO.Directory]::Move($stageRoot, $outputPath)

[pscustomobject]@{
    Status = 'PASS'
    PackageVersion = $Version
    PackageArchive = "$outputRelativePath/$packageName"
    PackageSHA256 = $zipHash
    ChecksumAsset = "$outputRelativePath/$checksumName"
    AuditReceipt = "$outputRelativePath/$receiptName"
    MemberCount = $packageMembers.Count
    ExecutableSHA256 = $binaryHash
    RuntimeInputsSHA256 = $runtimeInputIdentity
} | Format-List
