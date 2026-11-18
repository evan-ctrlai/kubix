#Requires -Version 7.0
[CmdletBinding()]
param(
    [ValidateSet('up', 'check', 'down')][string]$Action = 'check',
    [string]$StateDirectory = '',
    [ValidatePattern('^(|[0-9]+)$')][string]$VulkanDevice = ''
)
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.Windows.psm1') -Force
Invoke-KubixLabWindowsModel -Action $Action -StateDirectory $StateDirectory -VulkanDevice $VulkanDevice
