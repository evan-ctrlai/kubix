#requires -Version 7.2
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('up', 'model-up', 'model-check', 'rbac', 'verify', 'run', 'status', 'repair', 'reset', 'down')]
    [string]$Action = 'status',
    [ValidateSet('docker', 'windows-gpu')]
    [string]$ModelRuntime = 'docker',
    [switch]$WithoutModel,
    [switch]$RemoveModels
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
Import-Module (Join-Path $PSScriptRoot 'Cluster.psm1') -Force
$paths = Get-LabPaths
$lock = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'model-lock.json') -Raw | ConvertFrom-Json
$modelPort = if ($ModelRuntime -eq 'windows-gpu') { 11436 } else { $lock.port }
$modelURL = "http://127.0.0.1:$modelPort/v1"
if ($RemoveModels -and $Action -ne 'down') { throw '-RemoveModels is only valid with down.' }
if ($RemoveModels -and $ModelRuntime -eq 'windows-gpu') { throw 'Native GPU cleanup retains its downloaded files; -RemoveModels applies only to the Docker model volume.' }
if ($WithoutModel -and $Action -notin @('run', 'verify')) { throw '-WithoutModel is only valid with run or verify.' }

function Invoke-SelectedLabModel {
    param([ValidateSet('up', 'check', 'down')][string]$ModelAction)
    if ($ModelRuntime -eq 'windows-gpu') {
        & (Join-Path $PSScriptRoot 'model-windows.ps1') -Action $ModelAction
    } else {
        & (Join-Path $PSScriptRoot 'model.ps1') -Action $ModelAction -StateDirectory $paths.StateDirectory -RemoveModels:$RemoveModels
    }
}

function Invoke-CheckedGo {
    param([Parameter(Mandatory)][string[]]$Arguments)
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go 1.26 or newer is required to build and validate this source checkout.' }
    Push-Location $paths.RepoRoot
    try {
        & go @Arguments
        if ($LASTEXITCODE -ne 0) { throw 'The Go build or validation failed; review the output above.' }
    } finally { Pop-Location }
}

function Show-LabStatus {
    $checks = Test-LabReadOnly
    $checks | Format-Table -AutoSize
    & kubectl --kubeconfig $paths.ReaderConfig --context $paths.ReaderContext --namespace $paths.Namespace --request-timeout=10s get pods,replicasets,deployments -l app.kubernetes.io/name=checkout
    if ($LASTEXITCODE -ne 0) { throw 'Unable to read lab workload status with reader credentials.' }
    Write-Host "Reader kubeconfig: $($paths.ReaderConfig)"
}

switch ($Action) {
    'up' {
        & (Join-Path $PSScriptRoot 'install-kind.ps1')
        Initialize-LabCluster | Out-Host
        Write-Host 'Incident ready. Next: pwsh ./labs/crashloop/lab.ps1 model-up'
    }
    'model-up' { Invoke-SelectedLabModel 'up' | Out-Host }
    'model-check' { Invoke-SelectedLabModel 'check' | Out-Host }
    'rbac' { Test-LabReadOnly | Format-Table -AutoSize }
    'status' { Show-LabStatus }
    'repair' { Set-LabScenario -State healthy | Out-Host }
    'reset' { Set-LabScenario -State broken | Out-Host }
    'down' {
        Invoke-SelectedLabModel 'down' | Out-Host
        Remove-LabCluster | Out-Host
    }
    { $_ -in @('run', 'verify') } {
        # Verify ownership and the actual reader permissions before selecting a
        # cluster or starting the UI. Never use the user's current kubeconfig.
        Assert-LabOwnership -Reader | Out-Null
        Test-LabReadOnly | Out-Host
        if (-not $WithoutModel) {
            Invoke-SelectedLabModel 'check' | Out-Host
        }
        $overrides = @{
            KUBIX_MODEL_BASE_URL = $null
            KUBIX_MODEL_NAME = $null
            KUBIX_MODEL_API_KEY = $null
        }
        if ($Action -eq 'verify') {
            $overrides.KUBIX_LAB_RUN = '1'
            $overrides.KUBIX_LAB_KUBECONFIG = $paths.ReaderConfig
            $overrides.KUBIX_LAB_CONTEXT = $paths.ReaderContext
            $overrides.KUBIX_LAB_NAMESPACE = $paths.Namespace
            $reportName = if ($ModelRuntime -eq 'windows-gpu') { 'validation-windows-gpu.json' } else { 'validation.json' }
            $overrides.KUBIX_LAB_REPORT = Join-Path $paths.StateDirectory $reportName
            $overrides.KUBIX_LAB_MODEL_URL = if ($WithoutModel) { $null } else { $modelURL }
            $overrides.KUBIX_LAB_MODEL = if ($WithoutModel) { $null } else { $lock.model }
        }
        $previousEnvironment = @{}
        try {
            foreach ($name in $overrides.Keys) {
                $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
                [Environment]::SetEnvironmentVariable($name, $overrides[$name], 'Process')
            }
            if ($Action -eq 'verify') {
                Invoke-CheckedGo -Arguments @('test', './internal/labtest', '-run', '^TestLiveCrashLoop$', '-count=1', '-timeout', '15m', '-v')
                Write-Host "Validation report: $($overrides.KUBIX_LAB_REPORT)"
            } else {
                # A second demo window must not overwrite a running executable
                # on Windows. Each launcher owns and cleans up its own build.
                $binaryDirectory = Join-Path $paths.StateDirectory 'bin'
                New-Item -ItemType Directory -Path $binaryDirectory -Force | Out-Null
                $binaryName = if ($IsWindows) { "kubix-$PID.exe" } else { "kubix-$PID" }
                $binary = Join-Path $binaryDirectory $binaryName
                $kubixArgs = @('--kubeconfig', $paths.ReaderConfig, '--context', $paths.ReaderContext, '--namespace', $paths.Namespace,
                    '--model-timeout', '300s', '--investigation-timeout', '360s')
                if (-not $WithoutModel) { $kubixArgs += @('--model-url', $modelURL, '--model', $lock.model) }
                try {
                    Invoke-CheckedGo -Arguments @('build', '-o', $binary, './cmd/kubix')
                    & $binary @kubixArgs
                    if ($LASTEXITCODE -ne 0) { throw 'Kubix exited with an error; run the lab from an interactive terminal.' }
                } finally {
                    if (Test-Path -LiteralPath $binary -PathType Leaf) { Remove-Item -LiteralPath $binary -Force }
                }
            }
        } finally {
            foreach ($name in $previousEnvironment.Keys) { [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process') }
        }
    }
}
