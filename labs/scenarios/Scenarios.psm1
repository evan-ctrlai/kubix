#requires -Version 7.2
Set-StrictMode -Version Latest
$ErrorActionPreference='Stop'
function Assert-ScenarioPrefix([string]$Prefix) {
 if ($Prefix -cnotmatch '^kubix-check-[a-f0-9]{12}$') { throw 'Invalid scenario fixture prefix.' }
}
function New-ScenarioFixtures {
 param([ValidateSet('stale-current','healthy-history','service-mismatch')][string]$Scenario,[string]$Prefix)
 Assert-ScenarioPrefix $Prefix
 $labels=@{'kubix-scenario-run'=$Prefix;app='validation-real'}
 $command=if($Scenario -eq 'stale-current'){'echo "STALE (cached): previous diagnosis claimed DATABASE_URL was missing"; echo "FATAL (current): release signature verification failed"; exit 42'}else{'echo "healthy replacement running"; sleep 900'}
 $pod=@{apiVersion='v1';kind='Pod';metadata=@{name="$Prefix-pod";namespace='kubix-lab';labels=$labels};spec=@{automountServiceAccountToken=$false;restartPolicy='Never';terminationGracePeriodSeconds=1;securityContext=@{runAsNonRoot=$true;runAsUser=65532;runAsGroup=65532;seccompProfile=@{type='RuntimeDefault'}};containers=@(@{name='app';image='busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec';imagePullPolicy='IfNotPresent';command=@('/bin/sh','-c',$command);ports=@(@{name='http';containerPort=8080});resources=@{requests=@{cpu='5m';memory='8Mi'};limits=@{cpu='100m';memory='32Mi'}};securityContext=@{allowPrivilegeEscalation=$false;readOnlyRootFilesystem=$true;capabilities=@{drop=@('ALL')}}})}}
 $pod
 if($Scenario -eq 'service-mismatch') { @{apiVersion='v1';kind='Service';metadata=@{name="$Prefix-svc";namespace='kubix-lab';labels=@{'kubix-scenario-run'=$Prefix}};spec=@{selector=@{app='validation-wrong'};ports=@(@{port=80;targetPort=8080})}} }
}
function Assert-ScenarioOwnedObject {
 param($Object,[string]$Prefix,[string]$Kind,[string]$Name,[string]$ExpectedUID='')
 Assert-ScenarioPrefix $Prefix
 if($Kind -notin @('Pod','Service') -or $Object.kind -cne $Kind -or $Object.metadata.name -cne $Name -or $Object.metadata.namespace -cne 'kubix-lab' -or $Object.metadata.labels.'kubix-scenario-run' -cne $Prefix -or -not $Object.metadata.uid) {throw 'Scenario object identity/ownership mismatch; refusing cleanup.'}
 if($ExpectedUID -and $Object.metadata.uid -cne $ExpectedUID){throw 'Scenario object UID changed; refusing cleanup.'}
 [string]$Object.metadata.uid
}
function New-ScenarioDeleteOptions([string]$UID) {
 if(-not $UID -or $UID -notmatch '^[A-Za-z0-9-]{1,128}$'){throw 'Invalid cleanup UID.'}
 @{apiVersion='v1';kind='DeleteOptions';gracePeriodSeconds=1;preconditions=@{uid=$UID};propagationPolicy='Background'}
}
Export-ModuleMember -Function Assert-ScenarioPrefix,New-ScenarioFixtures,Assert-ScenarioOwnedObject,New-ScenarioDeleteOptions
