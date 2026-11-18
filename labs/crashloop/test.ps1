#requires -Version 7.2
# Static parse plus behavioral safety tests; no downloads, Docker, or Kubernetes.
$ErrorActionPreference = 'Stop'
$files = @(Get-ChildItem -LiteralPath $PSScriptRoot -File | Where-Object { $_.Extension -in @('.ps1', '.psm1') })
foreach ($file in $files) {
    $tokens = $null
    $parseErrors = $null
    $null = [Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0) { throw "PowerShell parse error in $($file.Name): $($parseErrors.Message -join '; ')" }
}
& (Join-Path $PSScriptRoot 'model.Tests.ps1')
& (Join-Path $PSScriptRoot 'cluster.Tests.ps1')
& (Join-Path $PSScriptRoot 'launch.Tests.ps1')
& (Join-Path $PSScriptRoot 'model-windows.Tests.ps1')
Write-Host 'Incident lab script checks passed without external services.'
# Guard cases intentionally execute native commands returning nonzero and assert
# their failures. Do not leak that expected native exit code to a caller such as
# GitHub Actions, which exits with LASTEXITCODE after this script completes.
# Any failed assertion above throws before this successful-suite reset.
$global:LASTEXITCODE = 0
