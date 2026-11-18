#requires -Version 7.2
[CmdletBinding()]
param(
 [Parameter(Mandatory)][ValidateSet('stale-current','healthy-history','service-mismatch')][string]$Scenario,
 [string]$ModelURL='',
 [string]$Model=''
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
Import-Module (Join-Path $PSScriptRoot '../crashloop/Cluster.psm1') -Force
Import-Module (Join-Path $PSScriptRoot 'Scenarios.psm1') -Force
$p=Assert-LabOwnership
$null=Assert-LabOwnership -Reader
$checks=@(Test-LabReadOnly)
$checks | Format-Table | Out-Host
if($Scenario -eq 'service-mismatch' -and @($checks | Where-Object {$_.Operation -eq 'list EndpointSlices' -and $_.Actual -eq 'no'}).Count -ne 1){throw 'Service-denial scenario requires existing EndpointSlice denial; permissions will not be changed.'}
if(($ModelURL -eq '') -ne ($Model -eq '')){throw 'Provide both ModelURL and Model or neither.'}
if($ModelURL){$uri=[uri]$ModelURL;if(-not $uri.IsLoopback -or $uri.Scheme -notin @('http','https') -or $uri.UserInfo -or $uri.Query -or $uri.Fragment -or $uri.AbsolutePath.TrimEnd('/') -ne '/v1'){throw 'Use a loopback local-model /v1 URL.'}}
$prefix='kubix-check-'+[guid]::NewGuid().ToString('N').Substring(0,12)
$workspace=Join-Path $p.StateDirectory $prefix
New-Item -ItemType Directory -Path $workspace | Out-Null
$planned=@(New-ScenarioFixtures -Scenario $Scenario -Prefix $prefix)
$created=@()
function Invoke-ScenarioKubectl([string[]]$Arguments) {
 # Revalidates isolated admin config hash for each operator call. Kubix itself
 # receives only the reader config and never this administrator credential.
 & (Get-Module Cluster) {param($a) Invoke-LabKubectl $a} $Arguments
}
Push-Location $p.RepoRoot
$failed=$null;$cleanupFailures=@()
try {
 foreach($fixture in $planned){
  $path=Join-Path $workspace ($fixture.kind.ToLower()+'.json')
  $fixture | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $path -Encoding utf8
  # Require absence before sending create, then retain a nonce-labelled cleanup
  # candidate even if the create response is lost after the API accepts it.
  $existing=Invoke-ScenarioKubectl @('-n','kubix-lab','get',"$($fixture.kind)/$($fixture.metadata.name)",'--ignore-not-found','-o','json')
  if($existing){throw 'Unique fixture name already exists; refusing to adopt it.'}
  $record=[pscustomobject]@{Kind=$fixture.kind;Name=$fixture.metadata.name;UID=''}
  $created+=$record
  $object=(Invoke-ScenarioKubectl @('create','-f',$path,'-o','json')) | ConvertFrom-Json
  $record.UID=Assert-ScenarioOwnedObject $object $prefix $fixture.kind $fixture.metadata.name
 }
 $condition=if($Scenario -eq 'stale-current'){'--for=jsonpath={.status.phase}=Failed'}else{'--for=condition=Ready'}
 $null=Invoke-ScenarioKubectl @('-n','kubix-lab','wait',$condition,"pod/$prefix-pod",'--timeout=90s')
 $args=@('run','./cmd/kubix-scenarios','--allow-live','--root',$p.RepoRoot,'--scenario',$Scenario,'--prefix',$prefix)
 if($ModelURL){$args+=@('--model-url',$ModelURL,'--model',$Model)}
 & go @args
 if($LASTEXITCODE -ne 0){throw 'Scenario validation failed; inspect its saved report.'}
} catch {$failed=$_} finally {
 foreach($record in $created){try{
  $resource=if($record.Kind -eq 'Pod'){'pods'}else{'services'}
  $raw=Invoke-ScenarioKubectl @('-n','kubix-lab','get',"$resource/$($record.Name)",'--ignore-not-found','-o','json')
  if(-not $raw){continue}
  $current=$raw | ConvertFrom-Json
  $uid=Assert-ScenarioOwnedObject $current $prefix $record.Kind $record.Name $record.UID
  $deletePath=Join-Path $workspace ('delete-'+$resource+'.json')
  New-ScenarioDeleteOptions $uid | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $deletePath -Encoding utf8
  # API UID precondition prevents deleting a replacement created after the GET.
  $null=Invoke-ScenarioKubectl @('delete','--raw',"/api/v1/namespaces/kubix-lab/$resource/$($record.Name)",'-f',$deletePath)
  $null=Invoke-ScenarioKubectl @('-n','kubix-lab','wait','--for=delete',"$resource/$($record.Name)",'--timeout=45s')
 }catch{$cleanupFailures+= "$($record.Kind)/$($record.Name): $($_.Exception.Message)"}}
 Pop-Location
}
if($cleanupFailures.Count){throw ('Exact fixture cleanup needs attention: '+($cleanupFailures -join '; '))}
Write-Host 'Owned scenario fixtures removed; existing lab workloads and RBAC were not changed.'
if($failed){throw $failed}
$global:LASTEXITCODE=0

