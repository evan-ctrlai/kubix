#Requires -Version 7.0
Set-StrictMode -Version Latest

$script:ModelLabRoot = $PSScriptRoot
$script:ModelRepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$script:OwnerLabel = 'io.kubix.lab.owner'
$script:WorkspaceLabel = 'io.kubix.lab.workspace'

function Get-KubixLabModelConfiguration {
    $configuration = Get-Content -LiteralPath (Join-Path $script:ModelLabRoot 'model-lock.json') -Raw | ConvertFrom-Json -AsHashtable
    $pathForHash = $script:ModelRepoRoot
    if ($IsWindows) { $pathForHash = $pathForHash.ToLowerInvariant() }
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { $hash = $hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($pathForHash)) } finally { $hasher.Dispose() }
    $configuration.workspace = [BitConverter]::ToString($hash).Replace('-', '').ToLowerInvariant().Substring(0, 24)
    $configuration.endpoint = "http://127.0.0.1:$($configuration.port)"
    return $configuration
}

function Assert-LabLocalDockerEndpoint {
    param([Parameter(Mandatory)][string]$Endpoint)
    # TCP, SSH and non-local named pipes are deliberately excluded, including
    # localhost TCP proxies whose actual destination cannot be established.
    if ($Endpoint -notmatch '^unix:///[^\r\n]+$' -and $Endpoint -notmatch '^npipe:////\./pipe/[^\r\n]+$') {
        throw 'The lab requires a local Docker Unix socket or local Windows named pipe. Remote Docker endpoints are not supported.'
    }
}

function Invoke-LabModelDocker {
    param([string[]]$DockerPrefix = @(), [Parameter(Mandatory)][string[]]$Arguments, [switch]$AllowMissing, [switch]$ShowProgress)
    if ($ShowProgress) {
        $lastProgress = [DateTime]::UtcNow.AddSeconds(-10)
        & docker @DockerPrefix @Arguments 2>&1 | ForEach-Object {
            # Native download tools redraw terminal progress frequently. Emit a
            # readable update periodically rather than buffering a multi-GB pull
            # until it finishes or flooding redirected logs with ANSI controls.
            $line = ([string]$_) -replace '\x1b\[[0-?]*[ -/]*[@-~]', '' -replace '[\x00-\x1f\x7f-\x9f]', ''
            $important = $line -match '(?i)complete|digest:|status:|success|error|failed'
            if ($line -and ($important -or ([DateTime]::UtcNow - $lastProgress).TotalSeconds -ge 3)) {
                Write-Host $line.Substring(0, [Math]::Min($line.Length, 250))
                $lastProgress = [DateTime]::UtcNow
            }
        }
        $output = @()
    } else {
        $output = & docker @DockerPrefix @Arguments 2>&1
    }
    if ($LASTEXITCODE -ne 0) {
        if ($AllowMissing -and (($output -join "`n") -match '(?i)no such (object|container|volume|image)')) { return $null }
        # Do not echo Docker's potentially sensitive configuration or full args.
        throw "Docker operation '$($Arguments[0])' failed. Confirm Docker Desktop is running with Linux containers."
    }
    return ($output -join "`n")
}

function Resolve-LabModelDocker {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        throw 'Docker is required for the isolated local model. Install/start Docker with Linux containers first.'
    }
    $prefix = @()
    $contextName = ''
    if ($env:DOCKER_CONTEXT) {
        $contextName = $env:DOCKER_CONTEXT
    } elseif ($env:DOCKER_HOST) {
        Assert-LabLocalDockerEndpoint $env:DOCKER_HOST
        $prefix = @('--host', $env:DOCKER_HOST)
    } else {
        $contextName = Invoke-LabModelDocker -Arguments @('context', 'show')
    }
    if ($contextName) {
        $contextInfo = Invoke-LabModelDocker -Arguments @('context', 'inspect', $contextName) | ConvertFrom-Json -AsHashtable -NoEnumerate
        Assert-LabLocalDockerEndpoint $contextInfo[0].Endpoints.docker.Host
        $prefix = @('--context', $contextName)
    }
    $server = Invoke-LabModelDocker -DockerPrefix $prefix -Arguments @('info', '--format', '{{json .}}') | ConvertFrom-Json -AsHashtable
    if ($server.OSType -ne 'linux') { throw 'The incident lab requires Docker Linux containers.' }
    return @{ Prefix = $prefix; Version = $server.ServerVersion; CPUs = $server.NCPU; MemoryBytes = $server.MemTotal }
}

function Assert-KubixLabModelVolume {
    param([Parameter(Mandatory)][hashtable]$Volume, [Parameter(Mandatory)][hashtable]$Configuration)
    if ($Volume.Name -ne $Configuration.volume -or -not $Volume.Labels -or
        $Volume.Labels[$script:OwnerLabel] -ne 'crashloop-model-v1' -or
        $Volume.Labels[$script:WorkspaceLabel] -ne $Configuration.workspace) {
        throw 'An unrelated model volume uses the lab name. It will not be reused or deleted.'
    }
}

function Assert-KubixLabModelContainerOwnership {
    param([Parameter(Mandatory)][hashtable]$Container, [Parameter(Mandatory)][hashtable]$Configuration)
    if ($Container.Name -ne ('/' + $Configuration.container) -or -not $Container.Config.Labels -or
        $Container.Config.Labels[$script:OwnerLabel] -ne 'crashloop-model-v1' -or
        $Container.Config.Labels[$script:WorkspaceLabel] -ne $Configuration.workspace) {
        throw 'An unrelated container uses the lab model name. It will not be reused or deleted.'
    }
    if (@($Container.Mounts).Count -ne 1 -or $Container.Mounts[0].Type -ne 'volume' -or
        $Container.Mounts[0].Name -ne $Configuration.volume -or $Container.Mounts[0].Destination -ne '/root/.ollama') {
        throw 'The model container has an unexpected mount. It will not be reused or deleted.'
    }
}

function Assert-KubixLabModelContainer {
    param([Parameter(Mandatory)][hashtable]$Container, [Parameter(Mandatory)][hashtable]$Configuration)
    Assert-KubixLabModelContainerOwnership $Container $Configuration
    if ($Container.Config.Image -ne $Configuration.image) { throw 'The existing model container does not use the locked image. Run pwsh ./labs/crashloop/model.ps1 -Action down before recreating.' }
    $ports = $Container.HostConfig.PortBindings
    if ($ports.Count -ne 1 -or -not $ports.ContainsKey('11434/tcp') -or
        @($ports['11434/tcp']).Count -ne 1 -or $ports['11434/tcp'][0].HostIp -ne '127.0.0.1' -or
        $ports['11434/tcp'][0].HostPort -ne [string]$Configuration.port -or $Container.HostConfig.NetworkMode -eq 'host') {
        throw 'The existing model container is not bound exclusively to the lab localhost port.'
    }
    foreach ($setting in @('OLLAMA_NO_CLOUD=1', 'OLLAMA_HOST=0.0.0.0:11434', "OLLAMA_CONTEXT_LENGTH=$($Configuration.context_length)", 'OLLAMA_NUM_PARALLEL=1')) {
        $settingName = $setting.Split('=')[0] + '='
        $values = @($Container.Config.Env | Where-Object { $_.StartsWith($settingName) })
        if ($values.Count -ne 1 -or $values[0] -ne $setting) { throw "The model container is missing or overrides required setting $setting." }
    }
}

function Get-LabModelObject {
    param([hashtable]$Docker, [string]$Kind, [string]$Name)
    $raw = Invoke-LabModelDocker -DockerPrefix $Docker.Prefix -Arguments @($Kind, 'inspect', $Name) -AllowMissing
    if ($null -eq $raw) { return $null }
    $objects = ConvertFrom-Json -InputObject $raw -AsHashtable -NoEnumerate
    return $objects[0]
}

function Invoke-LabModelRequest {
    param([hashtable]$Configuration, [string]$Path, [hashtable]$Body, [int]$TimeoutSeconds = 15)
    $parameters = @{ Uri = $Configuration.endpoint + $Path; TimeoutSec = $TimeoutSeconds; NoProxy = $true; MaximumRedirection = 0; ErrorAction = 'Stop' }
    if ($Body) {
        $parameters.Method = 'Post'
        $parameters.ContentType = 'application/json'
        $parameters.Body = ConvertTo-Json -InputObject $Body -Depth 12 -Compress
    }
    return Invoke-RestMethod @parameters
}

function Assert-LabModelDigest {
    param([Parameter(Mandatory)][string]$Actual, [Parameter(Mandatory)][hashtable]$Configuration)
    if (-not $Actual.StartsWith('sha256:')) { $Actual = 'sha256:' + $Actual }
    if ($Actual -ne $Configuration.model_digest) {
        throw 'The installed model digest differs from model-lock.json. Do not use this run as the pinned lab; review and intentionally update the lock before retrying.'
    }
}

function Test-KubixLabModel {
    param([hashtable]$Docker, [hashtable]$Configuration, [string]$StateDirectory)
    $container = Get-LabModelObject $Docker 'container' $Configuration.container
    if (-not $container) { throw 'The isolated lab model is not running. Run model-up first.' }
    Assert-KubixLabModelContainer $container $Configuration
    if (-not $container.State.Running) { throw 'The lab model container is stopped. Run model-up first.' }
    $version = Invoke-LabModelRequest $Configuration '/api/version'
    if ($version.version -ne $Configuration.server_version) { throw 'The model server version does not match the pinned lab image.' }
    $tags = Invoke-LabModelRequest $Configuration '/api/tags'
    $matchingModels = @($tags.models | Where-Object { $_.name -eq $Configuration.model })
    if ($matchingModels.Count -ne 1) { throw 'The pinned lab model is not installed. Run model-up first.' }
    Assert-LabModelDigest $matchingModels[0].digest $Configuration
    $timer = [Diagnostics.Stopwatch]::StartNew()
    $response = Invoke-LabModelRequest $Configuration '/v1/chat/completions' @{
        model = $Configuration.model
        messages = @(@{ role = 'user'; content = 'Reply with exactly KUBIX_LAB_READY and no other text.' })
        stream = $false; temperature = 0; max_tokens = 64
    } 300
    $timer.Stop()
    if (-not $response.choices -or $response.choices[0].message.role -ne 'assistant' -or
        [string]::IsNullOrWhiteSpace($response.choices[0].message.content) -or
        $response.choices[0].message.content -notmatch 'KUBIX_LAB_READY') {
        throw 'The local chat-completion probe did not return its expected marker. This is not a validated model connection.'
    }
    $loaded = Invoke-LabModelRequest $Configuration '/api/ps'
    $report = [ordered]@{
        checked_at = [DateTimeOffset]::UtcNow.ToString('o')
        endpoint = $Configuration.endpoint + '/v1'
        model = $Configuration.model
        model_digest = $Configuration.model_digest
        model_size_bytes = $matchingModels[0].size
        ollama_version = $version.version
        image = $Configuration.image
        image_id = $container.Image
        context_length = $Configuration.context_length
        cloud_disabled = $true
        docker_version = $Docker.Version
        docker_cpus = $Docker.CPUs
        docker_memory_bytes = $Docker.MemoryBytes
        container_cpus = $Configuration.cpus
        container_memory_limit = $Configuration.memory
        probe_elapsed_ms = $timer.ElapsedMilliseconds
        probe_passed = $true
        diagnostic_quality_validated = $false
        loaded_models = @($loaded.models | Select-Object name, digest, size, size_vram, context_length)
    }
    [IO.Directory]::CreateDirectory($StateDirectory) | Out-Null
    $report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $StateDirectory 'model-validation.json') -Encoding utf8
    Write-Host 'Local model probe passed. This validates connectivity and inference, not incident diagnosis quality.'
    return [PSCustomObject]$report
}

function Invoke-KubixLabModel {
    [CmdletBinding()]
    param([ValidateSet('up', 'check', 'down')][string]$Action = 'check', [string]$StateDirectory = '', [switch]$RemoveModels)
    $ErrorActionPreference = 'Stop'
    if ($RemoveModels -and $Action -ne 'down') { throw '-RemoveModels applies only to model-down.' }
    $configuration = Get-KubixLabModelConfiguration
    if (-not $StateDirectory) { $StateDirectory = Join-Path $script:ModelRepoRoot '.local/lab' }
    $StateDirectory = [IO.Path]::GetFullPath($StateDirectory)
    $localRoot = [IO.Path]::GetFullPath((Join-Path $script:ModelRepoRoot '.local')) + [IO.Path]::DirectorySeparatorChar
    $comparison = if ($IsWindows) { [StringComparison]::OrdinalIgnoreCase } else { [StringComparison]::Ordinal }
    if (-not $StateDirectory.StartsWith($localRoot, $comparison)) { throw 'Lab state must stay inside this checkout .local directory.' }
    $docker = Resolve-LabModelDocker
    $container = Get-LabModelObject $docker 'container' $configuration.container
    $volume = Get-LabModelObject $docker 'volume' $configuration.volume
    if ($volume) { Assert-KubixLabModelVolume $volume $configuration }
    if ($container) {
        if ($Action -eq 'down') {
            # A lock update must not strand an owned old container. Cleanup is
            # constrained by ownership and mount identity, not inference settings.
            Assert-KubixLabModelContainerOwnership $container $configuration
            if (-not $volume) { throw 'The model container volume ownership could not be verified; nothing will be removed.' }
        } else {
            Assert-KubixLabModelContainer $container $configuration
        }
    }
    if ($Action -eq 'down') {
        if ($container) { Invoke-LabModelDocker $docker.Prefix @('container', 'rm', '--force', $configuration.container) | Out-Null }
        if ($RemoveModels -and $volume) { Invoke-LabModelDocker $docker.Prefix @('volume', 'rm', $configuration.volume) | Out-Null }
        Write-Host 'Lab model stopped. Downloaded weights are retained unless -RemoveModels was specified.'
        return
    }
    if ($Action -eq 'up') {
        if (-not $volume) {
            Invoke-LabModelDocker $docker.Prefix @('volume', 'create', '--label', "$($script:OwnerLabel)=crashloop-model-v1", '--label', "$($script:WorkspaceLabel)=$($configuration.workspace)", $configuration.volume) | Out-Null
        }
        if (-not $container) {
            $image = Get-LabModelObject $docker 'image' $configuration.image
            if (-not $image) {
                Write-Host 'Downloading the pinned Ollama image (~3.4 GB compressed on AMD64; first use can take several minutes)…'
                Invoke-LabModelDocker -DockerPrefix $docker.Prefix -Arguments @('pull', $configuration.image) -ShowProgress | Out-Null
            }
            Write-Host 'Creating the isolated localhost model server…'
            Invoke-LabModelDocker $docker.Prefix @('run', '--pull', 'never', '--detach', '--name', $configuration.container,
                '--label', "$($script:OwnerLabel)=crashloop-model-v1", '--label', "$($script:WorkspaceLabel)=$($configuration.workspace)",
                '--publish', "127.0.0.1:$($configuration.port):11434", '--mount', "type=volume,src=$($configuration.volume),dst=/root/.ollama",
                '--cpus', [string]$configuration.cpus, '--memory', $configuration.memory, '--pids-limit', '256',
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                '--env', 'OLLAMA_NO_CLOUD=1', '--env', 'OLLAMA_HOST=0.0.0.0:11434', '--env', "OLLAMA_CONTEXT_LENGTH=$($configuration.context_length)",
                '--env', 'OLLAMA_KEEP_ALIVE=30m', '--env', 'OLLAMA_NUM_PARALLEL=1', '--env', 'OLLAMA_MAX_LOADED_MODELS=1', $configuration.image) | Out-Null
        } elseif (-not $container.State.Running) {
            Invoke-LabModelDocker $docker.Prefix @('container', 'start', $configuration.container) | Out-Null
        }
        $ready = $false
        for ($attempt = 0; $attempt -lt 30; $attempt++) {
            try { $null = Invoke-LabModelRequest $configuration '/api/version' $null 2; $ready = $true; break } catch { Start-Sleep -Seconds 1 }
        }
        if (-not $ready) { throw 'The local model server did not become ready within its startup budget.' }
        $tags = Invoke-LabModelRequest $configuration '/api/tags'
        $installed = @($tags.models | Where-Object { $_.name -eq $configuration.model })
        if ($installed.Count -eq 0) {
            Write-Host "Downloading $($configuration.model) (~1.9 GB on first use)…"
            Invoke-LabModelDocker -DockerPrefix $docker.Prefix -Arguments @('exec', $configuration.container, 'ollama', 'pull', $configuration.model) -ShowProgress | Out-Null
        } elseif ($installed.Count -eq 1) {
            Assert-LabModelDigest $installed[0].digest $configuration
        } else { throw 'The model list contains an ambiguous duplicate. The lab will not select one.' }
    }
    return Test-KubixLabModel $docker $configuration $StateDirectory
}

Export-ModuleMember -Function Get-KubixLabModelConfiguration, Assert-LabLocalDockerEndpoint, Assert-KubixLabModelVolume, Assert-KubixLabModelContainer, Assert-LabModelDigest, Invoke-KubixLabModel
