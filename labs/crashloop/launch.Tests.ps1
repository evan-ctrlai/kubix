#requires -Version 7.2
# No terminal window, Docker daemon, model, or cluster is accessed.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
Import-Module (Join-Path $PSScriptRoot 'Launch.psm1') -Force
$module = Get-Module Launch
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$testRoot = Join-Path $repoRoot ('.local/launch-tests-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $testRoot -Force
$saved = @{}
$names = @('NO_COLOR', 'TERM', 'COLORTERM', 'CLICOLOR', 'GOMODCACHE', 'GOCACHE', 'GOTMPDIR')
foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }

function Assert-Equal($Actual, $Expected, [string]$Message) {
    if ($Actual -ne $Expected) { throw "$Message (expected '$Expected', got '$Actual')" }
}

try {
    & $module {
        $script:Calls = @()
        $script:FailRun = $false
        function script:Invoke-LabRun {
            param([string]$RepoRoot, [string]$ModelRuntime, [switch]$WithoutModel)
            $script:Calls += [pscustomobject]@{
                NO_COLOR = $env:NO_COLOR; TERM = $env:TERM; COLORTERM = $env:COLORTERM; CLICOLOR = $env:CLICOLOR
                GOMODCACHE = $env:GOMODCACHE; GOCACHE = $env:GOCACHE; GOTMPDIR = $env:GOTMPDIR
                Directory = (Get-Location).Path; Runtime = $ModelRuntime; WithoutModel = [bool]$WithoutModel
            }
            if ($script:FailRun) { throw 'simulated lab failure' }
        }
    }
    $env:NO_COLOR = '1'; $env:TERM = 'dumb'; $env:COLORTERM = ''; $env:CLICOLOR = '0'
    $env:GOMODCACHE = 'inherited-modules'; $env:GOCACHE = 'inherited-cache'; $env:GOTMPDIR = 'inherited-temp'
    $previousLocation = (Get-Location).Path
    & $module { param($root) Invoke-LabTerminalSession -RepoRoot $root -ModelRuntime docker } $testRoot
    $plain = & $module { $script:Calls[-1] }
    Assert-Equal $plain.NO_COLOR '1' 'Default launch changed explicit NO_COLOR'
    Assert-Equal $plain.TERM 'dumb' 'Default launch changed terminal settings'
    Assert-Equal $plain.GOMODCACHE 'inherited-modules' 'Missing workspace module cache replaced existing setting'
    Assert-Equal $plain.GOCACHE 'inherited-cache' 'Missing workspace build cache replaced existing setting'
    Assert-Equal $plain.GOTMPDIR 'inherited-temp' 'Missing workspace temporary directory replaced existing setting'

    $null = New-Item -ItemType Directory -Path (Join-Path $testRoot '.local/go-cache') -Force
    & $module { param($root) Invoke-LabTerminalSession -RepoRoot $root -ModelRuntime windows-gpu -Color -WithoutModel } $testRoot
    $color = & $module { $script:Calls[-1] }
    Assert-Equal ([string]$color.NO_COLOR) '' 'Color opt-in retained NO_COLOR'
    Assert-Equal $color.TERM 'xterm-256color' 'Color opt-in retained dumb terminal'
    Assert-Equal $color.COLORTERM 'truecolor' 'Color opt-in did not advertise truecolor'
    Assert-Equal $color.CLICOLOR '1' 'Color opt-in retained disabled colors'
    Assert-Equal $color.GOCACHE (Join-Path $testRoot '.local/go-cache') 'Existing workspace cache was not selected'
    Assert-Equal $color.Runtime 'windows-gpu' 'Selected model runtime was not forwarded'
    Assert-Equal $color.WithoutModel $true 'Evidence-only option was not forwarded'
    Assert-Equal $color.Directory $testRoot 'Lab started outside the checkout'
    Assert-Equal $env:NO_COLOR '1' 'Color opt-in leaked into the parent session'
    Assert-Equal $env:TERM 'dumb' 'Terminal settings were not restored'
    Assert-Equal $env:GOCACHE 'inherited-cache' 'Workspace cache setting was not restored'
    Assert-Equal (Get-Location).Path $previousLocation 'Working directory was not restored'

    & $module { $script:FailRun = $true }
    $threw = $false
    try { & $module { param($root) Invoke-LabTerminalSession -RepoRoot $root -ModelRuntime docker -Color } $testRoot } catch {
        if ($_.Exception.Message -ne 'simulated lab failure') { throw }
        $threw = $true
    }
    Assert-Equal $threw $true 'Lab failure was swallowed'
    Assert-Equal $env:NO_COLOR '1' 'Failed launch leaked color settings'
    Assert-Equal $env:GOCACHE 'inherited-cache' 'Failed launch leaked cache settings'
    Assert-Equal (Get-Location).Path $previousLocation 'Failed launch leaked working directory'

    $window = @(& $module { param($root)
        Get-LabWindowArguments -RepoRoot $root -PwshPath 'C:/Program Files/PowerShell/7/pwsh.exe' `
            -Columns 160 -Rows 45 -ModelRuntime windows-gpu -Color -WithoutModel
    } $testRoot)
    Assert-Equal $window[1] 'new' 'Launcher reused an existing window'
    Assert-Equal $window[3] '160,45' 'Window dimensions changed'
    Assert-Equal $window[6] 'Kubix Lab - Evidence' 'Window title omitted the lab'
    Assert-Equal $window[9] $testRoot 'Starting directory was not a distinct argument'
    Assert-Equal $window[10] 'C:/Program Files/PowerShell/7/pwsh.exe' 'PowerShell path was split at spaces'
    Assert-Equal $window[15] (Join-Path $testRoot 'labs/crashloop/launch.ps1') 'Child launch does not use the supported script'
    Assert-Equal $window[17] 'windows-gpu' 'Window launch dropped selected runtime'
    Assert-Equal $window[18] '-Color' 'Window launch dropped explicit color opt-in'
    Assert-Equal $window[19] '-WithoutModel' 'Window launch dropped evidence-only mode'
    $gpuWindow = @(& $module { param($root)
        Get-LabWindowArguments -RepoRoot $root -PwshPath 'pwsh' -Columns 160 -Rows 45 -ModelRuntime windows-gpu
    } $testRoot)
    Assert-Equal $gpuWindow[6] 'Kubix Lab - GPU' 'GPU window cannot be distinguished from Docker'
    Assert-Equal ($gpuWindow -contains '-Color') $false 'Window launch enabled colors without opt-in'
    Assert-Equal ($gpuWindow -contains '-NewWindow') $false 'Child launch would recursively create windows'
    Write-Host 'PASS: lab launcher scopes color opt-in, preserves normal settings, forwards runtime, and restores failed sessions.'
} finally {
    foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') }
    Remove-Module Launch -ErrorAction SilentlyContinue
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $allowed = [IO.Path]::GetFullPath((Join-Path $repoRoot '.local')) + [IO.Path]::DirectorySeparatorChar
    if (-not $resolved.StartsWith($allowed, [StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing cleanup outside the test workspace.' }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
