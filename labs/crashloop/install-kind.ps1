#requires -Version 7.0
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# Checksums published by the official kind v0.31.0 release. Install only into
# the repository's ignored tool directory; never alter system PATH or packages.
$checksums = @{
    'windows-amd64' = '2c3a9ff954de16244380778683cf99e271bfc2fac9c6c4e797e4623c45e59d9d'
    'linux-amd64' = 'eb244cbafcc157dff60cf68693c14c9a75c4e6e6fedaf9cd71c58117cb93e3fa'
    'linux-arm64' = '8e1014e87c34901cc422a1445866835d1e666f2a61301c27e722bdeab5a1f7e4'
    'darwin-amd64' = 'a8b3cf77b2ad77aec5bf710d1a2589d9117576132af812885cad41e9dede4d4e'
    'darwin-arm64' = '88bf554fe9da6311c9f8c2d082613c002911a476f6b5090e9420b35d84e70c5c'
}
$osName = if ($IsWindows) { 'windows' } elseif ($IsMacOS) { 'darwin' } else { 'linux' }
$architecture = switch ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'X64' { 'amd64' }
    'Arm64' { 'arm64' }
    default { throw 'The lab bootstrap supports x64 and ARM64 hosts.' }
}
$platform = "$osName-$architecture"
if (-not $checksums.ContainsKey($platform)) { throw "No pinned kind binary for $platform." }
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$toolDirectory = Join-Path $repoRoot '.local/bin'
New-Item -ItemType Directory -Path $toolDirectory -Force | Out-Null
$fileName = if ($IsWindows) { 'kind.exe' } else { 'kind' }
$destination = Join-Path $toolDirectory $fileName
if ((Test-Path -LiteralPath $destination) -and ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant() -eq $checksums[$platform])) {
    Write-Host "Verified kind v0.31.0 already installed at $destination"
    return
}
$temporary = Join-Path $toolDirectory "kind-$([Guid]::NewGuid().ToString('N')).download"
try {
    Invoke-WebRequest -Uri "https://github.com/kubernetes-sigs/kind/releases/download/v0.31.0/kind-$platform" -OutFile $temporary -TimeoutSec 180
    if ((Get-FileHash -LiteralPath $temporary -Algorithm SHA256).Hash.ToLowerInvariant() -ne $checksums[$platform]) {
        throw 'Downloaded kind checksum does not match the pinned official release.'
    }
    Move-Item -LiteralPath $temporary -Destination $destination -Force
    if (-not $IsWindows) {
        & chmod 700 $destination
        if ($LASTEXITCODE -ne 0) { throw 'Unable to mark kind executable.' }
    }
    Write-Host "Installed verified kind v0.31.0 at $destination"
} finally {
    if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
}
