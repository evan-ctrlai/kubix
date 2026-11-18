#requires -Version 7.2
# Offline-only fixtures, ownership and parse tests. No cluster/model access.
$ErrorActionPreference='Stop'
Import-Module (Join-Path $PSScriptRoot 'Scenarios.psm1') -Force
function Must-Throw([scriptblock]$Action){try{& $Action}catch{return};throw 'Expected guard rejection.'}
$prefix='kubix-check-123456abcdef'
foreach($scenario in @('stale-current','healthy-history','service-mismatch')){
 $fixtures=@(New-ScenarioFixtures $scenario $prefix)
 $expected=if($scenario -eq 'service-mismatch'){2}else{1}
 if($fixtures.Count -ne $expected){throw 'Scenario is not independent.'}
 foreach($f in $fixtures){if($f.metadata.namespace -ne 'kubix-lab' -or $f.metadata.labels.'kubix-scenario-run' -ne $prefix){throw 'Unscoped fixture'};if($f.kind -eq 'Pod' -and ($f.spec.automountServiceAccountToken -ne $false -or $f.spec.containers[0].securityContext.allowPrivilegeEscalation -ne $false)){throw 'Fixture privileges changed'}}
}
Must-Throw {New-ScenarioFixtures 'healthy-history' '../bad'}
$o=[pscustomobject]@{kind='Pod';metadata=[pscustomobject]@{name="$prefix-pod";namespace='kubix-lab';uid='original-uid';labels=@{'kubix-scenario-run'=$prefix}}}
if((Assert-ScenarioOwnedObject $o $prefix 'Pod' "$prefix-pod" 'original-uid') -ne 'original-uid'){throw 'Lost UID'}
Must-Throw {Assert-ScenarioOwnedObject $o $prefix 'Pod' "$prefix-pod" 'replacement-uid'}
Must-Throw {Assert-ScenarioOwnedObject $o $prefix 'Service' "$prefix-pod"}
$o.metadata.labels.'kubix-scenario-run'='other';Must-Throw {Assert-ScenarioOwnedObject $o $prefix 'Pod' "$prefix-pod"}
$d=New-ScenarioDeleteOptions 'original-uid';if($d.preconditions.uid -ne 'original-uid' -or $d.kind -ne 'DeleteOptions'){throw 'Cleanup lacks server UID precondition'}
Must-Throw {New-ScenarioDeleteOptions ''}
foreach($file in Get-ChildItem $PSScriptRoot -File | Where-Object Extension -in @('.ps1','.psm1')){$tokens=$null;$errors=$null;$null=[Management.Automation.Language.Parser]::ParseFile($file.FullName,[ref]$tokens,[ref]$errors);if($errors.Count){throw "Parse failure: $($file.Name)"}}
Write-Host 'Scenario offline checks PASS; no live fixture or model was used.'
$global:LASTEXITCODE=0
