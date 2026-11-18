#requires -Version 7.2
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-LabPaths {
    $repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
    $stateDirectory = Join-Path $repoRoot '.local/lab'
    [pscustomobject]@{
        RepoRoot = $repoRoot
        LabRoot = $PSScriptRoot
        StateDirectory = $stateDirectory
        StateFile = Join-Path $stateDirectory 'cluster-state.json'
        AdminConfig = Join-Path $stateDirectory 'admin.kubeconfig'
        ReaderConfig = Join-Path $stateDirectory 'reader.kubeconfig'
        ClusterName = 'kubix-lab'
        ReaderContext = 'kubix-lab-reader'
        Namespace = 'kubix-lab'
    }
}

function Invoke-LabCommand {
    param([string]$File, [string[]]$Arguments, [switch]$AllowDenied)
    $PSNativeCommandUseErrorActionPreference = $false
    $errorFile = [IO.Path]::GetTempFileName()
    try {
        # Keep warnings separate from the machine-readable authorization result.
        $output = @(& $File @Arguments 2>$errorFile)
        $exitCode = $LASTEXITCODE
        $stderr = [IO.File]::ReadAllText($errorFile).Trim()
    } finally {
        Remove-Item -LiteralPath $errorFile -Force
    }
    $text = ($output | ForEach-Object { "$_" }) -join "`n"
    $onlyWarnings = @($stderr.Split("`n", [StringSplitOptions]::RemoveEmptyEntries) | Where-Object { -not $_.StartsWith('Warning: ') }).Count -eq 0
    if ($exitCode -ne 0 -and -not ($AllowDenied -and $exitCode -eq 1 -and $text.Trim() -eq 'no' -and $onlyWarnings)) {
        # Never include the argument list: another caller may pass credentials.
        throw "$([IO.Path]::GetFileName($File)) failed (exit $exitCode): $text $stderr"
    }
    $text.Trim()
}

function Get-LabExecutable {
    param([string]$Name)
    if ($Name -eq 'kind') {
        $fileName = if ($IsWindows) { 'kind.exe' } else { 'kind' }
        $local = Join-Path (Get-LabPaths).RepoRoot ".local/bin/$fileName"
        if (Test-Path -LiteralPath $local -PathType Leaf) { return $local }
    }
    $command = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $command) { throw "Missing prerequisite: $Name. See labs/crashloop/README.md." }
    $command.Source
}

function Get-LabDockerSession {
    # A dedicated cluster name is insufficient if Docker targets a remote host.
    if ($env:DOCKER_HOST -or $env:DOCKER_TLS_VERIFY) {
        throw 'Unset DOCKER_HOST and DOCKER_TLS_VERIFY for this local-only lab; use a local Docker context.'
    }
    if ($env:KIND_EXPERIMENTAL_PROVIDER -and $env:KIND_EXPERIMENTAL_PROVIDER -ne 'docker') {
        throw 'This lab supports the Docker kind provider only.'
    }
    $docker = Get-LabExecutable docker
    $context = Invoke-LabCommand $docker @('context', 'show')
    $endpoint = Invoke-LabCommand $docker @('context', 'inspect', $context, '--format', '{{.Endpoints.docker.Host}}')
    if (-not ($endpoint -cmatch '^unix:///[^\r\n]+$' -or $endpoint -cmatch '^npipe:////\./pipe/[^\r\n]+$')) {
        throw "Docker context '$context' is not a local Unix socket or Windows named pipe; no lab changes were made."
    }
    $info = Invoke-LabCommand $docker @('--context', $context, 'info', '--format', '{{.ID}}|{{.OSType}}')
    $parts = $info.Split('|')
    if ($parts.Count -ne 2 -or -not $parts[0] -or $parts[1] -ne 'linux') {
        throw 'The lab requires a running local Docker engine using Linux containers.'
    }
    [pscustomobject]@{ Executable = $docker; Context = $context; Endpoint = $endpoint; EngineID = $parts[0] }
}

function Get-LabNodeIDs {
    param($Session)
    $text = Invoke-LabCommand $Session.Executable @('--context', $Session.Context, 'ps', '-a', '--no-trunc', '--filter', 'label=io.x-k8s.kind.cluster=kubix-lab', '--format', '{{.ID}}')
    @($text.Split("`n", [StringSplitOptions]::RemoveEmptyEntries) | ForEach-Object { $_.Trim() } | Sort-Object)
}

function Protect-LabFile {
    param([string]$Path)
    if ($IsWindows) {
        $identity = [Security.Principal.WindowsIdentity]::GetCurrent().User
        # Request and modify only the DACL. Replacing owner/audit descriptors via
        # Set-Acl can require SeSecurityPrivilege even for a file we just made.
        $file = [IO.FileInfo]::new($Path)
        $acl = [IO.FileSystemAclExtensions]::GetAccessControl($file, [Security.AccessControl.AccessControlSections]::Access)
        $acl.SetAccessRuleProtection($true, $false)
        foreach ($existing in $acl.GetAccessRules($true, $false, [Security.Principal.SecurityIdentifier])) {
            $acl.RemoveAccessRuleSpecific($existing)
        }
        $rule = [Security.AccessControl.FileSystemAccessRule]::new($identity, 'FullControl', 'Allow')
        $acl.AddAccessRule($rule)
        [IO.FileSystemAclExtensions]::SetAccessControl($file, $acl)
    } else {
        [IO.File]::SetUnixFileMode($Path, [IO.UnixFileMode]::UserRead -bor [IO.UnixFileMode]::UserWrite)
    }
}

function Save-LabState {
    param($State)
    $paths = Get-LabPaths
    $State | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $paths.StateFile -Encoding utf8
    Protect-LabFile $paths.StateFile
}

function Read-LabState {
    $paths = Get-LabPaths
    if (-not (Test-Path -LiteralPath $paths.StateFile -PathType Leaf)) {
        throw 'This checkout has no ownership marker for kubix-lab. Run the lab setup; an existing unowned cluster will not be adopted.'
    }
    $state = Get-Content -LiteralPath $paths.StateFile -Raw | ConvertFrom-Json
    if ($state.Version -ne 1 -or $state.ClusterName -ne 'kubix-lab' -or $state.Namespace -ne 'kubix-lab' -or -not $state.OwnerID) {
        throw 'The lab ownership marker is invalid; refusing to use its credentials or modify a cluster.'
    }
    $state
}

function Assert-LabOwned {
    param($Session, [switch]$AllowMissing)
    $state = Read-LabState
    if ($state.DockerContext -ne $Session.Context -or $state.DockerEndpoint -ne $Session.Endpoint -or $state.EngineID -ne $Session.EngineID) {
        throw 'Docker context or engine differs from the recorded lab owner. Select the original local Docker context before continuing.'
    }
    $actual = @(Get-LabNodeIDs $Session)
    if ($actual.Count -eq 0 -and $AllowMissing) { return $state }
    $expected = @($state.NodeIDs | Sort-Object)
    if ($actual.Count -eq 0 -or $expected.Count -eq 0 -or ($actual -join ',') -ne ($expected -join ',')) {
        throw 'The kubix-lab containers do not match the ownership marker. No cluster changes were made.'
    }
    $state
}

function Invoke-LabKind {
    param($Session, [string[]]$Arguments)
    $paths = Get-LabPaths
    $kind = Get-LabExecutable kind
    $savedConfig, $savedProvider, $savedContext = $env:KUBECONFIG, $env:KIND_EXPERIMENTAL_PROVIDER, $env:DOCKER_CONTEXT
    try {
        # Even kind deletion may touch kubeconfig; isolate every invocation.
        $env:KUBECONFIG = $paths.AdminConfig
        $env:KIND_EXPERIMENTAL_PROVIDER = 'docker'
        $env:DOCKER_CONTEXT = $Session.Context
        Invoke-LabCommand $kind $Arguments
    } finally {
        $env:KUBECONFIG, $env:KIND_EXPERIMENTAL_PROVIDER, $env:DOCKER_CONTEXT = $savedConfig, $savedProvider, $savedContext
    }
}

function Invoke-LabKubectl {
    param([string[]]$Arguments, [switch]$Reader, [switch]$AllowDenied)
    $paths = Get-LabPaths
    $config = if ($Reader) { $paths.ReaderConfig } else { $paths.AdminConfig }
    $context = if ($Reader) { $paths.ReaderContext } else { 'kind-kubix-lab' }
    if (-not (Test-Path -LiteralPath $config -PathType Leaf)) { throw "Missing isolated lab kubeconfig: $config" }
    Assert-LabConfigIntegrity -Reader:$Reader
    Invoke-LabCommand (Get-LabExecutable kubectl) (@('--kubeconfig', $config, '--context', $context, '--request-timeout=10s') + $Arguments) -AllowDenied:$AllowDenied
}

function Assert-LabConfigIntegrity {
    param([switch]$Reader)
    $paths = Get-LabPaths
    $state = Read-LabState
    $key = if ($Reader) { 'ReaderConfigSHA256' } else { 'AdminConfigSHA256' }
    $path = if ($Reader) { $paths.ReaderConfig } else { $paths.AdminConfig }
    $property = $state.PSObject.Properties[$key]
    if (-not $property -or -not $property.Value -or -not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $property.Value) {
        throw 'The isolated lab kubeconfig is missing or changed since it was issued. Run lab up to regenerate credentials; no API request was sent.'
    }
}

function Assert-LabOwnership {
    param([switch]$Reader)
    $null = Assert-LabOwned (Get-LabDockerSession)
    Assert-LabConfigIntegrity -Reader:$Reader
    Get-LabPaths
}

function New-LabReaderConfig {
    $paths = Get-LabPaths
    # Read only cluster transport fields, never serialize admin user credentials.
    $cluster = Invoke-LabKubectl @('config', 'view', '--minify', '--raw', '-o', 'jsonpath={.clusters[0].cluster}') | ConvertFrom-Json
    $server = [uri]$cluster.server
    if ($server.Scheme -ne 'https' -or -not $server.IsLoopback -or -not $cluster.'certificate-authority-data') {
        throw 'Expected a loopback HTTPS kind API with embedded CA data; refusing to create reader credentials.'
    }
    $token = Invoke-LabKubectl @('-n', 'kubix-lab', 'create', 'token', 'kubix-reader', '--duration=2h')
    if ([string]::IsNullOrWhiteSpace($token) -or $token.Contains("`n")) { throw 'Failed to obtain a single short-lived reader token.' }
    $reader = @{
        apiVersion = 'v1'; kind = 'Config'; 'current-context' = $paths.ReaderContext
        clusters = @(@{ name = 'kubix-lab'; cluster = @{ server = $cluster.server; 'certificate-authority-data' = $cluster.'certificate-authority-data' } })
        users = @(@{ name = 'kubix-reader'; user = @{ token = $token } })
        contexts = @(@{ name = $paths.ReaderContext; context = @{ cluster = 'kubix-lab'; user = 'kubix-reader'; namespace = 'kubix-lab' } })
    }
    $reader | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $paths.ReaderConfig -Encoding utf8
    Protect-LabFile $paths.ReaderConfig
    $state = Read-LabState
    $state | Add-Member -NotePropertyName ReaderConfigSHA256 -NotePropertyValue (Get-FileHash -LiteralPath $paths.ReaderConfig -Algorithm SHA256).Hash -Force
    $state | Add-Member -NotePropertyName ReaderIssuedAt -NotePropertyValue ([DateTimeOffset]::UtcNow.ToString('o')) -Force
    Save-LabState $state
    $token = $null
}

function Test-LabReadOnly {
    $session = Get-LabDockerSession
    $null = Assert-LabOwned $session
    $checks = @(
        @{ Name = 'list pods'; Args = @('list', 'pods'); Expected = 'yes' },
        @{ Name = 'get pods'; Args = @('get', 'pods'); Expected = 'yes' },
        @{ Name = 'read pod logs'; Args = @('get', 'pods', '--subresource=log'); Expected = 'yes' },
        @{ Name = 'list events'; Args = @('list', 'events'); Expected = 'yes' },
        @{ Name = 'list EndpointSlices'; Args = @('list', 'endpointslices.discovery.k8s.io'); Expected = 'optional' },
        @{ Name = 'get EndpointSlices'; Args = @('get', 'endpointslices.discovery.k8s.io'); Expected = 'optional' },
        @{ Name = 'get ReplicaSets'; Args = @('get', 'replicasets.apps'); Expected = 'yes' },
        @{ Name = 'get Deployments'; Args = @('get', 'deployments.apps'); Expected = 'yes' },
        @{ Name = 'create pods'; Args = @('create', 'pods'); Expected = 'no' },
        @{ Name = 'patch deployments'; Args = @('patch', 'deployments.apps'); Expected = 'no' },
        @{ Name = 'update deployments'; Args = @('update', 'deployments.apps'); Expected = 'no' },
        @{ Name = 'delete pods'; Args = @('delete', 'pods'); Expected = 'no' },
        @{ Name = 'delete pod collections'; Args = @('deletecollection', 'pods'); Expected = 'no' },
        @{ Name = 'exec'; Args = @('create', 'pods', '--subresource=exec'); Expected = 'no' },
        @{ Name = 'port-forward'; Args = @('create', 'pods', '--subresource=portforward'); Expected = 'no' },
        @{ Name = 'get Secrets'; Args = @('get', 'secrets'); Expected = 'no' },
        @{ Name = 'list Secrets'; Args = @('list', 'secrets'); Expected = 'no' },
        @{ Name = 'change RBAC'; Args = @('create', 'rolebindings.rbac.authorization.k8s.io'); Expected = 'no' },
        @{ Name = 'mint tokens'; Args = @('create', 'serviceaccounts', '--subresource=token'); Expected = 'no' },
        @{ Name = 'list nodes'; Args = @('list', 'nodes'); Expected = 'no' }
    )
    $results = @()
    foreach ($check in $checks) {
        $namespaceArgs = if ($check.Name -eq 'list nodes') { @() } else { @('-n', 'kubix-lab') }
        $actual = Invoke-LabKubectl ($namespaceArgs + @('auth', 'can-i') + $check.Args) -Reader -AllowDenied
        $passed = if ($check.Expected -eq 'optional') { $actual -in @('yes', 'no') } else { $actual -eq $check.Expected }
        if (-not $passed) { throw "Read-only RBAC verification failed for '$($check.Name)': expected $($check.Expected), got $actual." }
        $expected = if ($check.Expected -eq 'optional') { 'yes or no (no means endpoint evidence is unavailable)' } else { $check.Expected }
        $results += [pscustomobject]@{ Operation = $check.Name; Expected = $expected; Actual = $actual; Passed = $passed }
    }
    $outside = Invoke-LabKubectl @('-n', 'default', 'auth', 'can-i', 'list', 'pods') -Reader -AllowDenied
    if ($outside -ne 'no') { throw 'Reader identity can list pods outside the lab namespace.' }
    $results += [pscustomobject]@{ Operation = 'list pods outside namespace'; Expected = 'no'; Actual = $outside; Passed = $true }
    $results
}

function Wait-LabCrashLoop {
    param([int]$TimeoutSeconds = 240)
    $null = Assert-LabOwned (Get-LabDockerSession)
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSeconds)
    Write-Host 'Waiting for a real CrashLoopBackOff with previous logs and a stable backoff window...'
    do {
        $list = Invoke-LabKubectl @('-n', 'kubix-lab', 'get', 'pods', '-l', 'app.kubernetes.io/name=checkout', '-o', 'json') | ConvertFrom-Json -AsHashtable
        foreach ($pod in $list.items) {
            if ($pod['metadata']['deletionTimestamp']) { continue }
            foreach ($container in @($pod['status']['containerStatuses'])) {
                if (-not $container) { continue }
                $waiting = $container['state']['waiting']
                if ($container.name -eq 'checkout' -and $waiting -and $waiting['reason'] -eq 'CrashLoopBackOff' -and $container.restartCount -ge 3) {
                    Write-Host "CrashLoop ready: $($pod.metadata.name) ($($container.restartCount) restarts)."
                    return [pscustomobject]@{ Pod = $pod.metadata.name; UID = $pod.metadata.uid; RestartCount = $container.restartCount }
                }
            }
        }
        Start-Sleep -Seconds 3
    } while ([DateTimeOffset]::UtcNow -lt $deadline)
    throw 'Timed out waiting for checkout to enter CrashLoopBackOff. Check image pull/network status with the isolated admin kubeconfig; the lab has not been deleted.'
}

function Initialize-LabCluster {
    $paths = Get-LabPaths
    $session = Get-LabDockerSession
    $version = Invoke-LabCommand (Get-LabExecutable kind) @('version')
    if ($version -notmatch '^kind v0\.31\.0\b') { throw 'Use kind v0.31.0 for this pinned lab.' }
    $null = Get-LabExecutable kubectl
    $actualNodes = @(Get-LabNodeIDs $session)
    if (Test-Path -LiteralPath $paths.StateFile -PathType Leaf) {
        $state = Assert-LabOwned $session -AllowMissing
    } elseif ($actualNodes.Count -ne 0) {
        throw 'A kubix-lab cluster already exists without this checkout ownership marker. It will not be adopted, reset, or deleted.'
    } else {
        $null = New-Item -ItemType Directory -Path $paths.StateDirectory -Force
        $state = [pscustomobject]@{ Version = 1; OwnerID = [guid]::NewGuid().ToString(); ClusterName = 'kubix-lab'; Namespace = 'kubix-lab'; DockerContext = $session.Context; DockerEndpoint = $session.Endpoint; EngineID = $session.EngineID; NodeIDs = @(); CreatedAt = [DateTimeOffset]::UtcNow.ToString('o') }
        Save-LabState $state
    }
    if ($actualNodes.Count -eq 0) {
        Write-Host 'Creating isolated kind cluster kubix-lab; your normal kubeconfig will not be changed.'
        $created = $false
        try {
            $null = Invoke-LabKind $session @('create', 'cluster', '--name', 'kubix-lab', '--config', (Join-Path $paths.LabRoot 'cluster.yaml'), '--kubeconfig', $paths.AdminConfig, '--wait', '120s')
            $created = $true
        } finally {
            # A failed create may mean another process won the name race. Never
            # adopt its containers, even if that leaves a partial lab to inspect.
            if ($created) {
                $state.NodeIDs = @(Get-LabNodeIDs $session)
                Save-LabState $state
            }
            if (Test-Path -LiteralPath $paths.AdminConfig -PathType Leaf) { Protect-LabFile $paths.AdminConfig }
        }
    }
    $null = Assert-LabOwned $session
    # Always refresh from the owned kind containers, never trust a locally edited
    # admin file. Overwrite via a fresh path so kind cannot merge extra users.
    $freshAdmin = Join-Path $paths.StateDirectory ('admin-' + [guid]::NewGuid().ToString('N') + '.kubeconfig')
    try {
        $null = Invoke-LabKind $session @('export', 'kubeconfig', '--name', 'kubix-lab', '--kubeconfig', $freshAdmin)
        Protect-LabFile $freshAdmin
        Copy-Item -LiteralPath $freshAdmin -Destination $paths.AdminConfig -Force
        Protect-LabFile $paths.AdminConfig
    } finally {
        if (Test-Path -LiteralPath $freshAdmin -PathType Leaf) { Remove-Item -LiteralPath $freshAdmin -Force }
    }
    $state | Add-Member -NotePropertyName AdminConfigSHA256 -NotePropertyValue (Get-FileHash -LiteralPath $paths.AdminConfig -Algorithm SHA256).Hash -Force
    Save-LabState $state
    $null = Invoke-LabKubectl @('apply', '-f', (Join-Path $paths.LabRoot 'manifests/namespace.yaml'))
    $null = Invoke-LabKubectl @('apply', '-f', (Join-Path $paths.LabRoot 'manifests/rbac.yaml'))
    New-LabReaderConfig
    $checks = @(Test-LabReadOnly)
    Write-Host "Read-only identity verified ($($checks.Count) authorization checks)."
    $null = Set-LabScenario -State broken
    $paths
}

function Set-LabScenario {
    param([Parameter(Mandatory)][ValidateSet('broken', 'healthy')][string]$State)
    $paths = Get-LabPaths
    $null = Assert-LabOwned (Get-LabDockerSession)
    if ($State -eq 'broken') {
        $null = Invoke-LabKubectl @('-n', 'kubix-lab', 'apply', '-f', (Join-Path $paths.LabRoot 'manifests/checkout.yaml'))
        # Explicit removal also resets a previous operator repair made with set env.
        $null = Invoke-LabKubectl @('-n', 'kubix-lab', 'set', 'env', 'deployment/checkout', 'DATABASE_URL-')
        Wait-LabCrashLoop
    } else {
        $null = Invoke-LabKubectl @('-n', 'kubix-lab', 'set', 'env', 'deployment/checkout', 'DATABASE_URL=demo://configured')
        $null = Invoke-LabKubectl @('-n', 'kubix-lab', 'rollout', 'status', 'deployment/checkout', '--timeout=90s')
        Write-Host 'Operator repair complete: checkout is healthy. Kubix did not execute this change.'
    }
}

function Remove-LabCluster {
    $paths = Get-LabPaths
    $session = Get-LabDockerSession
    if (-not (Test-Path -LiteralPath $paths.StateFile -PathType Leaf)) {
        if (@(Get-LabNodeIDs $session).Count -eq 0) {
            Write-Host 'The lab cluster is already absent; nothing was removed.'
            return
        }
        throw 'A kubix-lab cluster exists without this checkout ownership marker; it will not be deleted.'
    }
    $null = Assert-LabOwned $session -AllowMissing
    $null = Invoke-LabKind $session @('delete', 'cluster', '--name', 'kubix-lab')
    foreach ($path in @($paths.ReaderConfig, $paths.AdminConfig, $paths.StateFile)) {
        if (Test-Path -LiteralPath $path -PathType Leaf) { Remove-Item -LiteralPath $path -Force }
    }
    Write-Host 'Removed the owned kubix-lab cluster and its credentials. Other Docker resources and kubeconfigs were left alone.'
}

Export-ModuleMember -Function Get-LabPaths, Assert-LabOwnership, Initialize-LabCluster, Set-LabScenario, Test-LabReadOnly, Wait-LabCrashLoop, Remove-LabCluster
