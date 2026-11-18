#Requires -Version 7.0
# No Docker daemon, cluster, model, Pester module, or network is used.
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.psm1') -Force

function Assert-Throws([scriptblock]$Run, [string]$Description) {
    $threw = $false
    try { & $Run | Out-Null } catch { $threw = $true }
    if (-not $threw) { throw "Expected rejection: $Description" }
}

$configuration = Get-KubixLabModelConfiguration
if ($configuration.model -ne 'qwen2.5:3b' -or $configuration.endpoint -ne 'http://127.0.0.1:11435' -or
    $configuration.image -notmatch '@sha256:[a-f0-9]{64}$' -or $configuration.model_digest -notmatch '^sha256:[a-f0-9]{64}$') {
    throw 'The lab model or image is not explicitly pinned.'
}

foreach ($endpoint in @('unix:///var/run/docker.sock', 'unix:///run/user/1000/docker.sock', 'npipe:////./pipe/dockerDesktopLinuxEngine')) {
    Assert-LabLocalDockerEndpoint $endpoint
}
foreach ($endpoint in @('tcp://example.org:2375', 'tcp://127.0.0.1:2375', 'ssh://user@host', 'npipe:////another-host/pipe/docker_engine', "unix:///sock`nremote")) {
    Assert-Throws { Assert-LabLocalDockerEndpoint $endpoint } "remote Docker endpoint $endpoint"
}

function New-ContainerFixture {
    return @{
        Name = '/' + $configuration.container
        Config = @{
            Image = $configuration.image
            Labels = @{ 'io.kubix.lab.owner' = 'crashloop-model-v1'; 'io.kubix.lab.workspace' = $configuration.workspace }
            Env = @('OLLAMA_NO_CLOUD=1', 'OLLAMA_HOST=0.0.0.0:11434', "OLLAMA_CONTEXT_LENGTH=$($configuration.context_length)", 'OLLAMA_NUM_PARALLEL=1')
        }
        HostConfig = @{ NetworkMode = 'bridge'; PortBindings = @{ '11434/tcp' = @(@{ HostIp = '127.0.0.1'; HostPort = '11435' }) } }
        Mounts = @(@{ Type = 'volume'; Name = $configuration.volume; Destination = '/root/.ollama' })
    }
}

$container = New-ContainerFixture
Assert-KubixLabModelContainer $container $configuration
foreach ($change in @(
    { param($c) $c.Config.Labels['io.kubix.lab.workspace'] = 'another-checkout' },
    { param($c) $c.Config.Labels = @{} },
    { param($c) $c.Config.Image = 'ollama/ollama:latest' },
    { param($c) $c.HostConfig.PortBindings['11434/tcp'][0].HostIp = '0.0.0.0' },
    { param($c) $c.HostConfig.NetworkMode = 'host' },
    { param($c) $c.HostConfig.PortBindings['9999/tcp'] = @(@{ HostIp = '127.0.0.1'; HostPort = '9999' }) },
    { param($c) $c.Config.Env += 'OLLAMA_NO_CLOUD=0' },
    { param($c) $c.Config.Env = @('OLLAMA_HOST=0.0.0.0:11434') },
    { param($c) $c.Mounts += @{ Type = 'bind'; Source = '/var/run/docker.sock'; Destination = '/var/run/docker.sock' } }
)) {
    $changed = New-ContainerFixture
    & $change $changed
    Assert-Throws { Assert-KubixLabModelContainer $changed $configuration } 'unsafe existing model container'
}

$volume = @{ Name = $configuration.volume; Labels = @{ 'io.kubix.lab.owner' = 'crashloop-model-v1'; 'io.kubix.lab.workspace' = $configuration.workspace } }
Assert-KubixLabModelVolume $volume $configuration
$volume.Labels['io.kubix.lab.workspace'] = 'unrelated'
Assert-Throws { Assert-KubixLabModelVolume $volume $configuration } 'unrelated named volume'
Assert-LabModelDigest $configuration.model_digest $configuration
Assert-LabModelDigest $configuration.model_digest.Substring(7) $configuration
Assert-Throws { Assert-LabModelDigest ('sha256:' + ('0' * 64)) $configuration } 'model tag drift'
# TEMP may legitimately point inside this checkout's .local directory. The repo
# root itself is always outside the allowed state subtree. Stub Docker resolution
# as well so a guard regression fails this test without contacting a real engine.
$guardModule = Get-Module Kubix.LabModel
$outsideState = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
try {
    & $guardModule {
        $script:GuardDockerCalls = 0
        function script:Resolve-LabModelDocker {
            $script:GuardDockerCalls++
            throw 'A preflight guard reached the mocked Docker boundary.'
        }
    }
    Assert-Throws { Invoke-KubixLabModel -StateDirectory $outsideState } 'state written outside checkout .local'
    if ((& $guardModule { $script:GuardDockerCalls }) -ne 0) { throw 'Invalid state path reached Docker resolution.' }
    Assert-Throws { Invoke-KubixLabModel -Action check -RemoveModels } 'destructive flag on read-only check'
    if ((& $guardModule { $script:GuardDockerCalls }) -ne 0) { throw 'Invalid cleanup flag reached Docker resolution.' }
} finally {
    Remove-Module Kubix.LabModel
    Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.psm1') -Force
}

# A new lock may intentionally change image/context settings. Owned old resources
# must remain removable, while startup and checks continue to reject that drift.
$cleanupContainer = New-ContainerFixture
$cleanupContainer.Config.Image = 'ollama/ollama:old-test-image'
$cleanupContainer.Config.Env = @('OLLAMA_NO_CLOUD=1', 'OLLAMA_HOST=0.0.0.0:11434', 'OLLAMA_CONTEXT_LENGTH=4096', 'OLLAMA_NUM_PARALLEL=1')
$cleanupVolume = @{ Name = $configuration.volume; Labels = @{ 'io.kubix.lab.owner' = 'crashloop-model-v1'; 'io.kubix.lab.workspace' = $configuration.workspace } }
$modelModule = Get-Module Kubix.LabModel
& $modelModule {
    param($Container, $Volume)
    $script:CleanupContainer = $Container
    $script:CleanupVolume = $Volume
    $script:CleanupOperations = @()
    function script:Resolve-LabModelDocker { @{ Prefix = @('--context', 'verified-local-test'); Version = 'test'; CPUs = 8; MemoryBytes = 16000000000 } }
    function script:Get-LabModelObject {
        param($Docker, $Kind, $Name)
        if ($Kind -eq 'container') { return $script:CleanupContainer }
        if ($Kind -eq 'volume') { return $script:CleanupVolume }
        throw 'Unexpected object lookup in cleanup test'
    }
    function script:Invoke-LabModelDocker {
        param($DockerPrefix, $Arguments)
        if (($DockerPrefix -join ' ') -ne '--context verified-local-test') { throw 'Cleanup lost verified Docker targeting' }
        $script:CleanupOperations += ($Arguments -join ' ')
    }
} $cleanupContainer $cleanupVolume
foreach ($action in @('up', 'check')) {
    Assert-Throws { Invoke-KubixLabModel -Action $action } "runtime drift accepted during $action"
}
if (@(& $modelModule { $script:CleanupOperations }).Count -ne 0) { throw 'Rejected startup attempted a mutation' }
Invoke-KubixLabModel -Action down
$operations = @(& $modelModule { $script:CleanupOperations })
if ($operations.Count -ne 1 -or $operations[0] -ne "container rm --force $($configuration.container)") { throw 'Default cleanup removed more than the known owned container' }
& $modelModule { $script:CleanupOperations = @() }
Invoke-KubixLabModel -Action down -RemoveModels
$operations = @(& $modelModule { $script:CleanupOperations })
if ($operations.Count -ne 2 -or $operations[0] -ne "container rm --force $($configuration.container)" -or $operations[1] -ne "volume rm $($configuration.volume)") { throw 'Explicit model cleanup used an unexpected removal target' }
foreach ($unsafe in @('workspace', 'name', 'bind-mount', 'volume-label', 'missing-volume')) {
    $fixture = New-ContainerFixture
    $fixture.Config.Image = 'old-test-image'
    $fixtureVolume = @{ Name = $configuration.volume; Labels = @{ 'io.kubix.lab.owner' = 'crashloop-model-v1'; 'io.kubix.lab.workspace' = $configuration.workspace } }
    switch ($unsafe) {
        'workspace' { $fixture.Config.Labels['io.kubix.lab.workspace'] = 'another-checkout' }
        'name' { $fixture.Name = '/another-container' }
        'bind-mount' { $fixture.Mounts += @{ Type='bind'; Source='/host/data'; Destination='/data' } }
        'volume-label' { $fixtureVolume.Labels['io.kubix.lab.workspace'] = 'another-checkout' }
        'missing-volume' { $fixtureVolume = $null }
    }
    & $modelModule { param($c, $v) $script:CleanupContainer = $c; $script:CleanupVolume = $v; $script:CleanupOperations = @() } $fixture $fixtureVolume
    Assert-Throws { Invoke-KubixLabModel -Action down -RemoveModels } "unsafe cleanup: $unsafe"
    if (@(& $modelModule { $script:CleanupOperations }).Count -ne 0) { throw "Unsafe cleanup mutated resources: $unsafe" }
}
# Reset mocks before exercising independent model-probe behavior below.
Remove-Module Kubix.LabModel
Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.psm1') -Force

# Exercise report construction after the regex-based inference probe. PowerShell
# updates its automatic $Matches variable on a match; model metadata must not
# accidentally be stored there and overwritten by the probe response.
$testContainer = New-ContainerFixture
$testContainer.State = @{ Running = $true }
$testContainer.Image = 'sha256:test-image'
$testState = Join-Path ([IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))) ('.local/model-test-' + [Guid]::NewGuid().ToString('N'))
try {
    $report = & (Get-Module Kubix.LabModel) {
        param($TestConfiguration, $TestContainer, $TestState)
        $script:MockConfiguration = $TestConfiguration
        $script:MockContainer = $TestContainer
        function script:Get-LabModelObject { return $script:MockContainer }
        function script:Invoke-LabModelRequest {
            param($Configuration, $Path, $Body, $TimeoutSeconds)
            switch ($Path) {
                '/api/version' { return [PSCustomObject]@{ version = $Configuration.server_version } }
                '/api/tags' { return [PSCustomObject]@{ models = @([PSCustomObject]@{ name = $Configuration.model; digest = $Configuration.model_digest; size = 1929912432 }) } }
                '/v1/chat/completions' { return [PSCustomObject]@{ choices = @([PSCustomObject]@{ message = [PSCustomObject]@{ role = 'assistant'; content = 'KUBIX_LAB_READY' } }) } }
                '/api/ps' { return [PSCustomObject]@{ models = @([PSCustomObject]@{ name = $Configuration.model; digest = $Configuration.model_digest; size = 2700000000; size_vram = 0; context_length = 16384 }) } }
                default { throw "Unexpected mocked request $Path" }
            }
        }
        return Test-KubixLabModel @{ Version = 'test'; CPUs = 8; MemoryBytes = 16000000000 } $TestConfiguration $TestState
    } $configuration $testContainer $testState
    if ($report.model_size_bytes -ne 1929912432 -or -not $report.probe_passed -or $report.diagnostic_quality_validated) {
        throw 'The inference probe lost model metadata or overstated its validation.'
    }
    $saved = Get-Content -LiteralPath (Join-Path $testState 'model-validation.json') -Raw | ConvertFrom-Json
    if ($saved.model_size_bytes -ne $report.model_size_bytes) { throw 'The saved model report differs from the actual probe result.' }
} finally {
    $testReport = Join-Path $testState 'model-validation.json'
    if (Test-Path -LiteralPath $testReport) { Remove-Item -LiteralPath $testReport }
    if (Test-Path -LiteralPath $testState) { Remove-Item -LiteralPath $testState }
}

Write-Host 'Model lab safety tests passed (no Docker or network used).'
