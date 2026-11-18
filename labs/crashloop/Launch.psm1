#requires -Version 7.2
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Invoke-LabRun {
    param([string]$RepoRoot, [string]$ModelRuntime, [switch]$WithoutModel)
    & (Join-Path $RepoRoot 'labs/crashloop/lab.ps1') run -ModelRuntime $ModelRuntime -WithoutModel:$WithoutModel
}

function Invoke-LabTerminalSession {
    param([string]$RepoRoot, [string]$ModelRuntime, [switch]$Color, [switch]$WithoutModel)
    $overrides = @{}
    if ($Color) {
        # Explicit opt-in for this lab session only. Normal Kubix launches still
        # respect NO_COLOR, including when inherited from a user's shell.
        $overrides.NO_COLOR = $null
        $overrides.TERM = 'xterm-256color'
        $overrides.COLORTERM = 'truecolor'
        $overrides.CLICOLOR = '1'
    }
    foreach ($entry in @{
        GOMODCACHE = '.local/go-mod'
        GOCACHE = '.local/go-cache'
        GOTMPDIR = '.local/go-tmp'
    }.GetEnumerator()) {
        $directory = Join-Path $RepoRoot $entry.Value
        if (Test-Path -LiteralPath $directory -PathType Container) { $overrides[$entry.Key] = $directory }
    }
    $previous = @{}
    Push-Location -LiteralPath $RepoRoot
    try {
        foreach ($name in $overrides.Keys) {
            $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
            [Environment]::SetEnvironmentVariable($name, $overrides[$name], 'Process')
        }
        Invoke-LabRun -RepoRoot $RepoRoot -ModelRuntime $ModelRuntime -WithoutModel:$WithoutModel
    } finally {
        foreach ($name in $previous.Keys) { [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process') }
        Pop-Location
    }
}

function Get-LabWindowArguments {
    param([string]$RepoRoot, [string]$PwshPath, [int]$Columns, [int]$Rows,
        [string]$ModelRuntime, [switch]$Color, [switch]$WithoutModel)
    $title = if ($WithoutModel) { 'Kubix Lab - Evidence' } elseif ($ModelRuntime -eq 'windows-gpu') { 'Kubix Lab - GPU' } else { 'Kubix Lab - Docker' }
    $arguments = @('--window', 'new', '--size', "$Columns,$Rows", 'new-tab',
        '--title', $title, '--suppressApplicationTitle', '--startingDirectory', $RepoRoot,
        $PwshPath, '-NoLogo', '-NoProfile', '-NoExit', '-File',
        (Join-Path $RepoRoot 'labs/crashloop/launch.ps1'), '-ModelRuntime', $ModelRuntime)
    if ($Color) { $arguments += '-Color' }
    if ($WithoutModel) { $arguments += '-WithoutModel' }
    return $arguments
}

function Start-KubixLabTerminal {
    [CmdletBinding()]
    param(
        [switch]$NewWindow,
        [switch]$Color,
        [switch]$WithoutModel,
        [ValidateSet('docker', 'windows-gpu')][string]$ModelRuntime = 'docker',
        [ValidateRange(100, 300)][int]$Columns = 160,
        [ValidateRange(30, 100)][int]$Rows = 45
    )
    $repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
    if ($NewWindow) {
        if (-not $IsWindows) { throw 'Open an interactive terminal and run launch.ps1 without -NewWindow on this platform.' }
        $terminal = Get-Command wt -CommandType Application -ErrorAction SilentlyContinue
        $shell = Get-Command pwsh -CommandType Application -ErrorAction SilentlyContinue
        if (-not $terminal -or -not $shell) { throw 'Windows Terminal (wt) and PowerShell 7.2+ (pwsh) must be on PATH, or run launch.ps1 in your current terminal.' }
        $arguments = Get-LabWindowArguments -RepoRoot $repoRoot -PwshPath $shell.Source -Columns $Columns -Rows $Rows `
            -ModelRuntime $ModelRuntime -Color:$Color -WithoutModel:$WithoutModel
        & $terminal.Source @arguments
        if ($LASTEXITCODE -ne 0) { throw 'Windows Terminal could not open the lab window.' }
        return
    }
    Invoke-LabTerminalSession -RepoRoot $repoRoot -ModelRuntime $ModelRuntime -Color:$Color -WithoutModel:$WithoutModel
}

Export-ModuleMember -Function Start-KubixLabTerminal
