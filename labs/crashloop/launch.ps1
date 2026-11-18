#requires -Version 7.2
[CmdletBinding()]
param(
    [switch]$NewWindow,
    [switch]$Color,
    [switch]$WithoutModel,
    [ValidateSet('docker', 'windows-gpu')][string]$ModelRuntime = 'docker',
    [ValidateRange(100, 300)][int]$Columns = 160,
    [ValidateRange(30, 100)][int]$Rows = 45
)
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'Launch.psm1') -Force
Start-KubixLabTerminal @PSBoundParameters
