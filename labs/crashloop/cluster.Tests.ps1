#requires -Version 7.2
# Behavioral guard tests. No Docker daemon, kubeconfig, or cluster is accessed.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$modulePath = Join-Path $PSScriptRoot 'Cluster.psm1'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$testDirectory = Join-Path $repoRoot ('.local/lab-tests-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $testDirectory -Force

function Assert-Equal($Actual, $Expected, [string]$Message) {
    if ($Actual -ne $Expected) { throw "$Message (expected '$Expected', got '$Actual')" }
}
function Assert-Throws([scriptblock]$Action, [string]$Pattern) {
    try { & $Action } catch {
        if ($_.Exception.Message -match $Pattern) { return }
        throw "Wrong failure: $($_.Exception.Message)"
    }
    throw "Expected failure matching '$Pattern'."
}
function New-TestModule {
    Remove-Module Cluster -ErrorAction SilentlyContinue
    Import-Module $modulePath -Force
    $module = Get-Module Cluster
    & $module {
        param($directory)
        $script:TestDirectory = $directory
        function script:Get-LabPaths {
            [pscustomobject]@{
                RepoRoot = $script:TestDirectory; LabRoot = $script:TestDirectory
                StateDirectory = $script:TestDirectory; StateFile = Join-Path $script:TestDirectory 'state.json'
                AdminConfig = Join-Path $script:TestDirectory 'admin.kubeconfig'
                ReaderConfig = Join-Path $script:TestDirectory 'reader.kubeconfig'
                ReaderContext = 'kubix-lab-reader'; ClusterName = 'kubix-lab'; Namespace = 'kubix-lab'
            }
        }
        function script:Protect-LabFile { param([string]$Path) }
        $script:TestState = [pscustomobject]@{ Version=1; ClusterName='kubix-lab'; Namespace='kubix-lab'; OwnerID='test-owner'; DockerContext='local'; DockerEndpoint='unix:///var/run/docker.sock'; EngineID='engine-A'; NodeIDs=@('node-A') }
        function script:Read-LabState { $script:TestState }
        function script:Save-LabState { param($State) $script:TestState = $State }
    } $testDirectory
    $module
}

$savedHost, $savedTLS, $savedProvider = $env:DOCKER_HOST, $env:DOCKER_TLS_VERIFY, $env:KIND_EXPERIMENTAL_PROVIDER
try {
    $env:DOCKER_HOST, $env:DOCKER_TLS_VERIFY, $env:KIND_EXPERIMENTAL_PROVIDER = $null, $null, $null
    Import-Module $modulePath -Force
    $module = Get-Module Cluster
    $protectedFile = Join-Path $testDirectory 'private-file-probe.txt'
    [IO.File]::WriteAllText($protectedFile, 'test only; no credentials')
    & $module { param($path) Protect-LabFile $path } $protectedFile
    if ($IsWindows) {
        $acl = [IO.FileSystemAclExtensions]::GetAccessControl([IO.FileInfo]::new($protectedFile), [Security.AccessControl.AccessControlSections]::Access)
        $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
        Assert-Equal $acl.AreAccessRulesProtected $true 'Credential file inherited broad access'
        Assert-Equal $rules.Count 1 'Credential file has extra access rules'
        Assert-Equal $rules[0].IdentityReference.Value ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value) 'Credential file grants another identity'
        Assert-Equal $rules[0].AccessControlType 'Allow' 'Credential owner does not have access'
        Assert-Equal $rules[0].FileSystemRights 'FullControl' 'Credential owner access changed'
    } else {
        Assert-Equal ([int][IO.File]::GetUnixFileMode($protectedFile)) ([int]([IO.UnixFileMode]::UserRead -bor [IO.UnixFileMode]::UserWrite)) 'Credential file mode is not 0600'
    }
    Write-Host 'PASS: private files are restricted to the current user without changing owner/audit metadata.'

    $module = New-TestModule
    $shellExecutable = (Get-Command pwsh -CommandType Application | Select-Object -First 1).Source
    $deniedWithWarning = & $module {
        param($shell)
        Invoke-LabCommand $shell @('-NoProfile', '-Command', "[Console]::Out.WriteLine('no'); [Console]::Error.WriteLine('Warning: resource is not namespace scoped'); exit 1") -AllowDenied
    } $shellExecutable
    Assert-Equal $deniedWithWarning 'no' 'Authorization warning corrupted the denied result'
    Assert-Throws {
        & $module {
            param($shell)
            Invoke-LabCommand $shell @('-NoProfile', '-Command', "[Console]::Out.WriteLine('no'); [Console]::Error.WriteLine('Error: connection failed'); exit 1") -AllowDenied
        } $shellExecutable
    } 'connection failed'
    Write-Host 'PASS: explicit authorization denials tolerate warnings but preserve genuine errors.'

    $module = New-TestModule
    & $module {
        function script:Get-LabExecutable { param($Name) $Name }
        $script:Endpoint = 'unix:///var/run/docker.sock'
        $script:InfoCalls = 0
        function script:Invoke-LabCommand {
            param($File, $Arguments)
            switch ($Arguments[0]) {
                'context' { if ($Arguments[1] -eq 'show') { 'local' } else { $script:Endpoint } }
                '--context' { $script:InfoCalls++; 'engine-A|linux' }
                default { throw 'Unexpected command' }
            }
        }
    }
    foreach ($endpoint in @('ssh://host', 'tcp://127.0.0.1:2375', 'unix://remote/socket', 'npipe:////remote/pipe/docker_engine', "unix:///socket`nremote")) {
        & $module { param($value) $script:Endpoint = $value } $endpoint
        Assert-Throws { & $module { Get-LabDockerSession } } 'not a local'
    }
    Assert-Equal (& $module { $script:InfoCalls }) 0 'Remote endpoint guard contacted engine'
    foreach ($endpoint in @('unix:///var/run/docker.sock', 'npipe:////./pipe/dockerDesktopLinuxEngine')) {
        & $module { param($value) $script:Endpoint = $value } $endpoint
        Assert-Equal (& $module { (Get-LabDockerSession).EngineID }) 'engine-A' 'Local endpoint was refused'
    }
    $env:DOCKER_HOST = 'ssh://remote'
    Assert-Throws { & $module { Get-LabDockerSession } } 'Unset DOCKER_HOST'
    $env:DOCKER_HOST = $null
    Write-Host 'PASS: remote Docker endpoints and environment overrides are refused before engine access.'

    $module = New-TestModule
    & $module {
        $script:ActualNodes = @('node-A')
        function script:Get-LabNodeIDs { param($Session) $script:ActualNodes }
    }
    $session = [pscustomobject]@{ Context='local'; Endpoint='unix:///var/run/docker.sock'; EngineID='engine-A' }
    $owned = & $module { param($session) Assert-LabOwned $session } $session
    Assert-Equal $owned.OwnerID 'test-owner' 'Owned cluster was rejected'
    & $module { $script:ActualNodes = @('replacement-node') }
    Assert-Throws { & $module { param($session) Assert-LabOwned $session } $session } 'do not match'
    & $module { $script:ActualNodes = @('node-A') }
    $session.EngineID = 'different-engine'
    Assert-Throws { & $module { param($session) Assert-LabOwned $session } $session } 'differs'
    Write-Host 'PASS: engine and node identity mismatches cannot be adopted.'

    $module = New-TestModule
    & $module {
        function script:Get-LabDockerSession { [pscustomobject]@{ Context='local'; EngineID='engine-A' } }
        function script:Get-LabExecutable { param($Name) $Name }
        function script:Invoke-LabCommand { param($File, $Arguments) if ($File -eq 'kind' -and $Arguments[0] -eq 'version') { return 'kind v0.31.0 test' }; throw 'Unexpected external operation' }
        function script:Get-LabNodeIDs { param($Session) @('unowned-node') }
    }
    Assert-Throws { & $module { Initialize-LabCluster } } 'without this checkout ownership marker'
    Write-Host 'PASS: an existing unowned lab name is rejected without mutation.'

    $module = New-TestModule
    & $module {
        function script:Get-LabDockerSession { [pscustomobject]@{ Context='local'; Endpoint='unix:///var/run/docker.sock'; EngineID='engine-A' } }
        function script:Get-LabExecutable { param($Name) $Name }
        function script:Invoke-LabCommand { param($File, $Arguments) 'kind v0.31.0 test' }
        $script:NodeReads = 0
        function script:Get-LabNodeIDs { param($Session) $script:NodeReads++; if ($script:NodeReads -gt 1) { @('another-process-node') } }
        function script:Invoke-LabKind { param($Session, $Arguments) throw 'Simulated already exists race' }
    }
    Assert-Throws { & $module { Initialize-LabCluster } } 'already exists race'
    Assert-Equal (& $module { @($script:TestState.NodeIDs).Count }) 0 'Failed create adopted another cluster'
    Assert-Equal (& $module { $script:NodeReads }) 1 'Failed create tried to claim newly appearing nodes'
    Write-Host 'PASS: failed kind creation never claims container ownership.'

    $module = New-TestModule
    & $module {
        function script:Get-LabDockerSession { 'local-session' }
        $script:ExistingNodes = @()
        function script:Get-LabNodeIDs { param($Session) $script:ExistingNodes }
        function script:Invoke-LabKind { throw 'Unexpected delete call' }
    }
    & $module { Remove-LabCluster }
    & $module { $script:ExistingNodes = @('unowned-node') }
    Assert-Throws { & $module { Remove-LabCluster } } 'without this checkout ownership marker'
    Write-Host 'PASS: repeated cleanup is harmless while unowned clusters stay protected.'

    $module = New-TestModule
    & $module {
        $script:Transport = @{ server='https://127.0.0.1:6443'; 'certificate-authority-data'='dGVzdC1jYQ=='; 'client-key-data'='ADMIN-KEY-SENTINEL' }
        $script:TokenCalls = 0
        function script:Invoke-LabKubectl {
            param($Arguments)
            if ($Arguments[0] -eq 'config') {
                if ($Arguments[-1] -ne 'jsonpath={.clusters[0].cluster}') { throw 'Admin user credentials requested' }
                return $script:Transport | ConvertTo-Json -Compress
            }
            $script:TokenCalls++
            if (($Arguments -join ' ') -ne '-n kubix-lab create token kubix-reader --duration=2h') { throw 'Unexpected token request' }
            'fake-short-lived-token'
        }
        New-LabReaderConfig
    }
    $readerPath = Join-Path $testDirectory 'reader.kubeconfig'
    $raw = Get-Content -LiteralPath $readerPath -Raw
    if ($raw.Contains('ADMIN-KEY-SENTINEL')) { throw 'Admin credential leaked into reader file' }
    $reader = $raw | ConvertFrom-Json -AsHashtable
    Assert-Equal $reader['current-context'] 'kubix-lab-reader' 'Wrong reader context'
    Assert-Equal ($reader.users[0].user.Keys -join ',') 'token' 'Reader has extra authentication mechanisms'
    Assert-Equal $reader.contexts[0].context.namespace 'kubix-lab' 'Reader escaped namespace'
    Assert-Equal (& $module { $script:TokenCalls }) 1 'Unexpected token calls'
    & $module { Assert-LabConfigIntegrity -Reader }
    Add-Content -LiteralPath $readerPath -Value 'tampered'
    Assert-Throws { & $module { Assert-LabConfigIntegrity -Reader } } 'missing or changed'
    & $module { $script:Transport.server = 'https://production.example.com'; $script:TokenCalls = 0 }
    Assert-Throws { & $module { New-LabReaderConfig } } 'loopback HTTPS'
    Assert-Equal (& $module { $script:TokenCalls }) 0 'Remote transport minted credentials'
    Write-Host 'PASS: reader credentials contain only scoped token identity; edited configs and remote transport are refused.'

    $module = New-TestModule
    & $module {
        function script:Get-LabDockerSession { 'local-session' }
        function script:Assert-LabOwned { param($Session) 'owned' }
        $script:Escalated = $false
        $script:AuthorizationCalls = @()
        function script:Invoke-LabKubectl {
            param($Arguments, [switch]$Reader, [switch]$AllowDenied)
            if (-not $Reader) { throw 'RBAC check used admin identity' }
            $query = $Arguments -join ' '
            $script:AuthorizationCalls += $query
            if ($query -match 'endpointslices.discovery.k8s.io') { return $script:EndpointSlices }
            if ($script:Escalated -and $query.Contains('patch deployments.apps')) { return 'yes' }
            if ($query -match '^\-n kubix-lab auth can-i (list pods|get pods|get pods --subresource=log|list events|list endpointslices.discovery.k8s.io|get endpointslices.discovery.k8s.io|get replicasets.apps|get deployments.apps)$') { return 'yes' }
            'no'
        }
        $script:EndpointSlices = 'yes'
    }
    $results = @(& $module { Test-LabReadOnly })
    Assert-Equal $results.Count 21 'Permission matrix incomplete'
    Assert-Equal (@($results | Where-Object { -not $_.Passed }).Count) 0 'Valid read-only matrix did not pass'
    & $module { $script:EndpointSlices = 'no' }
    $optionalResults = @(& $module { Test-LabReadOnly })
    $endpointChecks = @($optionalResults | Where-Object { $_.Operation -in @('list EndpointSlices', 'get EndpointSlices') })
    Assert-Equal $endpointChecks.Count 2 'EndpointSlice read availability was not reported'
    Assert-Equal (@($endpointChecks | Where-Object { $_.Actual -ne 'no' -or -not $_.Passed }).Count) 0 'Denied optional EndpointSlice reads should be reported as unavailable without blocking safe browsing'
    & $module { $script:EndpointSlices = 'yes' }
    & $module { $script:Escalated = $true }
    Assert-Throws { & $module { Test-LabReadOnly } } 'patch deployments'
    Write-Host 'PASS: authorization probes use reader credentials and detect unexpected mutation grants.'

    $module = New-TestModule
    $savedConfig, $savedContext = $env:KUBECONFIG, $env:DOCKER_CONTEXT
    try {
        $env:KUBECONFIG, $env:DOCKER_CONTEXT = 'user-original-config', 'original-docker'
        & $module {
            function script:Get-LabExecutable { param($Name) $Name }
            function script:Invoke-LabCommand {
                param($File, $Arguments)
                if ($env:KUBECONFIG -ne (Get-LabPaths).AdminConfig -or $env:DOCKER_CONTEXT -ne 'owned-docker' -or $env:KIND_EXPERIMENTAL_PROVIDER -ne 'docker') { throw 'Kind environment not isolated' }
                throw 'Simulated kind failure'
            }
        }
        Assert-Throws { & $module { Invoke-LabKind ([pscustomobject]@{ Context='owned-docker' }) @('delete', 'cluster', '--name', 'kubix-lab') } } 'Simulated kind failure'
        Assert-Equal $env:KUBECONFIG 'user-original-config' 'User kubeconfig environment changed'
        Assert-Equal $env:DOCKER_CONTEXT 'original-docker' 'User Docker context environment changed'
    } finally { $env:KUBECONFIG, $env:DOCKER_CONTEXT = $savedConfig, $savedContext }
    Write-Host 'PASS: kind create/delete environment stays isolated, including failure paths.'
    Write-Host 'All cluster guard tests passed; no Docker or Kubernetes operations were performed.'
} finally {
    $env:DOCKER_HOST, $env:DOCKER_TLS_VERIFY, $env:KIND_EXPERIMENTAL_PROVIDER = $savedHost, $savedTLS, $savedProvider
    Remove-Module Cluster -ErrorAction SilentlyContinue
    $resolvedTest = [IO.Path]::GetFullPath($testDirectory)
    $allowedParent = [IO.Path]::GetFullPath((Join-Path $repoRoot '.local')) + [IO.Path]::DirectorySeparatorChar
    if (-not $resolvedTest.StartsWith($allowedParent, [StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing unexpected test cleanup path.' }
    Remove-Item -LiteralPath $resolvedTest -Recurse -Force
}
