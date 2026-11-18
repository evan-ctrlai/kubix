#Requires -Version 7.0
[CmdletBinding()]
param(
    [ValidateSet('up', 'check', 'down')]
    [string]$Action = 'check',
    [string]$StateDirectory = '',
    [switch]$RemoveModels
)

$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.psm1') -Force
Invoke-KubixLabModel -Action $Action -StateDirectory $StateDirectory -RemoveModels:$RemoveModels
