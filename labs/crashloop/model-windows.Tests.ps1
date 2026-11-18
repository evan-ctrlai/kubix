#Requires -Version 7.0
# Guard tests only: no services, network, GPU work, or runtime downloads.
$ErrorActionPreference = 'Stop'
if (-not $IsWindows) {
    Write-Host 'Native Windows model behavioral guards skipped on this OS; shared script parsing still runs.'
    return
}
Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.Windows.psm1') -Force
$nativeModule = Get-Module Kubix.LabModel.Windows
$configuration = Get-KubixLabWindowsModelConfiguration

function Invoke-NativeTestFunction([string]$Name, [object[]]$Arguments) {
    & $nativeModule { param($Name, $Arguments) & $Name @Arguments } $Name $Arguments
}
function Assert-NativeThrows([scriptblock]$Run, [string]$Description) {
    $threw = $false
    try { & $Run | Out-Null } catch { $threw = $true }
    if (-not $threw) { throw "Expected rejection: $Description" }
}
function New-NativeMarkerFixture {
    @{
        owner = 'crashloop-windows-model-v1'; workspace = $configuration.workspace
        pid = 12345; executable = $configuration.executable; executable_sha256 = ('a' * 64)
        started_at = '2026-11-13T19:30:07.1920915Z'; endpoint = $configuration.endpoint
        model_directory = $configuration.model_directory; server_version = $configuration.server_version
        environment = (Invoke-NativeTestFunction Get-NativeEnvironment @($configuration))
        stdout = $configuration.stdout; stderr = $configuration.stderr
    }
}
function New-NativeProcessFixture {
    $m = New-NativeMarkerFixture
    @{ pid = $m.pid; executable = $m.executable; started_at = $m.started_at; command_line = ('"' + $m.executable + '" serve') }
}

if ($configuration.endpoint -ne 'http://127.0.0.1:11436' -or $configuration.native_pin.server_version -ne $configuration.server_version) {
    throw 'Native runtime is not pinned to the shared local model configuration.'
}
$environment = Invoke-NativeTestFunction Get-NativeEnvironment @($configuration, '0')
if ($environment.OLLAMA_KEEP_ALIVE -ne '5m' -or $environment.OLLAMA_NO_CLOUD -ne '1' -or $environment.OLLAMA_HOST -ne '127.0.0.1:11436') {
    throw 'Native environment lost its idle-unload, local-only, or cloud-disabled boundary.'
}
foreach ($name in @('USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'TEMP', 'TMP', 'OLLAMA_TMPDIR', 'OLLAMA_MODELS')) {
    $null = Invoke-NativeTestFunction Assert-NativeLabPath @($environment[$name])
}
foreach ($path in @($PSScriptRoot, (Join-Path $PSScriptRoot '../../.local-other/file'), (Join-Path $configuration.state_directory '../../../outside'))) {
    Assert-NativeThrows { Invoke-NativeTestFunction Assert-NativeLabPath @($path) } 'path outside the local workspace subtree'
}
$zipDestination = Join-Path $configuration.state_directory 'archive-test'
$null = Invoke-NativeTestFunction Resolve-NativeZipEntry @($zipDestination, 'lib/ollama/runner.dll', 0)
foreach ($name in @('../escape.dll', 'lib/../../escape.dll', '\\server\share\evil', '/escape', 'C:\escape', 'lib\..\escape.dll', 'file:stream')) {
    Assert-NativeThrows { Invoke-NativeTestFunction Resolve-NativeZipEntry @($zipDestination, $name, 0) } "ZIP traversal or alternate stream $name"
}
Assert-NativeThrows { Invoke-NativeTestFunction Resolve-NativeZipEntry @($zipDestination, 'linked-file', 2684354560) } 'ZIP Unix symlink'

$marker = New-NativeMarkerFixture
Invoke-NativeTestFunction Assert-NativeMarker @($marker, $configuration)
$marker.environment.GGML_VK_VISIBLE_DEVICES = '0'
Invoke-NativeTestFunction Assert-NativeMarker @($marker, $configuration)
foreach ($change in @(
    { param($m) $m.owner = 'unrelated' },
    { param($m) $m.workspace = 'other-checkout' },
    { param($m) $m.executable = 'C:\unrelated\ollama.exe' },
    { param($m) $m.endpoint = 'http://example.org:11436' },
    { param($m) $m.endpoint = 'http://0.0.0.0:11436' },
    { param($m) $m.model_directory = 'C:\outside' },
    { param($m) $m.stderr = 'C:\outside.log' },
    { param($m) $m.environment.OLLAMA_NO_CLOUD = '0' },
    { param($m) $m.environment.USERPROFILE = 'C:\Users\Unrelated' },
    { param($m) $m.environment.OLLAMA_KEEP_ALIVE = '30m' },
    { param($m) $m.environment.GGML_VK_VISIBLE_DEVICES = '0,1' },
    { param($m) $m.environment.OLLAMA_REMOTE_HOST = 'outside.example' },
    { param($m) $m.server_version = '0.0.0' }
)) {
    $changed = New-NativeMarkerFixture
    & $change $changed
    Assert-NativeThrows { Invoke-NativeTestFunction Assert-NativeMarker @($changed, $configuration) } 'unsafe native ownership/configuration marker'
}
$process = New-NativeProcessFixture
Invoke-NativeTestFunction Assert-NativeProcess @($process, (New-NativeMarkerFixture))
# Exercise the actual JSON decoder: it may produce a typed UTC DateTime rather
# than a string. PID checks must retain its sub-microsecond start-time precision.
$decodedMarker = New-NativeMarkerFixture | ConvertTo-Json -Depth 8 | ConvertFrom-Json -AsHashtable
Invoke-NativeTestFunction Assert-NativeMarker @($decodedMarker, $configuration)
Invoke-NativeTestFunction Assert-NativeProcess @($process, $decodedMarker)
foreach ($change in @(
    { param($p) $p.pid = 12346 },
    { param($p) $p.executable = 'C:\unrelated\ollama.exe' },
    { param($p) $p.started_at = '2026-11-13T20:30:07Z' },
    { param($p) $p.command_line += ' --other-service' },
    { param($p) $p.command_line = 'ollama.exe serve' }
)) {
    $changed = New-NativeProcessFixture
    & $change $changed
    Assert-NativeThrows { Invoke-NativeTestFunction Assert-NativeProcess @($changed, (New-NativeMarkerFixture)) } 'recycled PID or changed native process identity'
}
Invoke-NativeTestFunction Assert-NativeListeners @(@(@{ address = '127.0.0.1'; pid = 12345 }), 12345)
foreach ($listener in @(@{ address = '0.0.0.0'; pid = 12345 }, @{ address = '::'; pid = 12345 }, @{ address = '127.0.0.1'; pid = 98765 })) {
    Assert-NativeThrows { Invoke-NativeTestFunction Assert-NativeListeners @(@($listener), 12345) } 'unowned or non-loopback listener'
}
$loaded = @{ name = $configuration.model; digest = $configuration.model_digest; size = 3000000000; size_vram = 2700000000; context_length = 16384 }
$proof = Invoke-NativeTestFunction Get-NativeGPUProof @($configuration, @{ models = @($loaded) })
if ($proof.gpu_allocated_bytes -ne 2700000000) { throw 'GPU proof did not preserve measured allocation.' }
$loaded.size_vram = 0
Assert-NativeThrows { Invoke-NativeTestFunction Get-NativeGPUProof @($configuration, @{ models = @($loaded) }) } 'CPU-only probe presented as GPU validation'
$loaded.size_vram = 2700000000
$loaded.digest = 'sha256:' + ('0' * 64)
Assert-NativeThrows { Invoke-NativeTestFunction Get-NativeGPUProof @($configuration, @{ models = @($loaded) }) } 'loaded model digest drift'
$remote = $configuration.Clone(); $remote.endpoint = 'http://example.org:11436'
Assert-NativeThrows { Invoke-NativeTestFunction Invoke-NativeRequest @($remote, '/api/version') } 'remote model request before network access'
Assert-NativeThrows { Invoke-NativeTestFunction Invoke-NativeRequest @($configuration, '/../api/version') } 'non-allowlisted API route before network access'

# Exercise lifecycle boundaries with every side-effecting dependency replaced.
# The real ownership, PID, environment, and listener checks stay active.
& $nativeModule {
    param($Marker, $Process)
    $script:FixtureMarker = $Marker; $script:FixtureProcess = $Process
    $script:FixtureDescendants = @()
    $script:FixtureListeners = @(@{ address = '127.0.0.1'; pid = $Marker.pid })
    $script:NativeOperations = @()
    function script:Read-NativeMarker { param($Path) $script:FixtureMarker }
    function script:Assert-NativeExecutable { param($Marker, $Configuration) }
    function script:Get-NativeProcess { param($ServerProcessId) $script:FixtureProcess }
    function script:Get-NativeListeners { param($Port) $script:FixtureListeners }
    function script:Get-NativeDescendants { param($Marker) $script:FixtureDescendants }
    function script:Invoke-NativeTreeStop {
        param($Marker)
        $script:NativeOperations += "stop-tree:$($Marker.pid)"
        $script:FixtureListeners = @(); $script:FixtureProcess = $null
    }
    function script:Remove-Item { param($LiteralPath) $script:NativeOperations += "remove:$LiteralPath" }
    function script:Install-NativeRuntime { throw 'Unexpected runtime installation in guard tests.' }
    function script:Test-NativeModel { throw 'Unexpected model inference in guard tests.' }
    function script:Invoke-NativeRequest { throw 'Unexpected network request in guard tests.' }
} (New-NativeMarkerFixture) (New-NativeProcessFixture)
try {
    Assert-NativeThrows { Invoke-KubixLabWindowsModel -Action up -StateDirectory $PSScriptRoot } 'state override outside .local'
    & $nativeModule { $script:FixtureProcess.started_at = '2026-11-13T20:30:07Z' }
    Assert-NativeThrows { Invoke-KubixLabWindowsModel -Action down } 'PID reuse during shutdown'
    if (@(& $nativeModule { $script:NativeOperations }).Count -ne 0) { throw 'Rejected PID reached shutdown mutation.' }
    & $nativeModule { param($Process) $script:FixtureProcess = $Process; $script:FixtureListeners[0].pid = 99999 } (New-NativeProcessFixture)
    Assert-NativeThrows { Invoke-KubixLabWindowsModel -Action down } 'unrelated listener during shutdown'
    if (@(& $nativeModule { $script:NativeOperations }).Count -ne 0) { throw 'Unrelated listener reached shutdown mutation.' }
    & $nativeModule {
        $script:FixtureProcess = $null; $script:FixtureListeners = @()
        $script:FixtureDescendants = @(@{ pid = 12346 })
    }
    Assert-NativeThrows { Invoke-KubixLabWindowsModel -Action down } 'dead server with remaining orphan runner'
    if (@(& $nativeModule { $script:NativeOperations }).Count -ne 0) { throw 'Orphan guard removed its ownership marker or stopped an unverified process.' }
    & $nativeModule {
        param($Process)
        $script:FixtureProcess = $Process; $script:FixtureListeners = @(@{ address = '127.0.0.1'; pid = 12345 })
        $script:FixtureDescendants = @()
    } (New-NativeProcessFixture)
    & $nativeModule { $script:FixtureListeners[0].pid = 12345; $script:FixtureMarker.environment.OLLAMA_KEEP_ALIVE = '30m' }
    Invoke-KubixLabWindowsModel -Action down
    $operations = @(& $nativeModule { $script:NativeOperations })
    if ($operations.Count -ne 2 -or $operations[0] -ne 'stop-tree:12345' -or $operations[1] -ne "remove:$($configuration.state_file)") {
        throw 'Shutdown did not restrict mutations to the owned process tree and state marker.'
    }
    & $nativeModule { $script:FixtureMarker = $null; $script:NativeOperations = @() }
    Invoke-KubixLabWindowsModel -Action down
    if (@(& $nativeModule { $script:NativeOperations }).Count -ne 0) { throw 'Repeated shutdown performed an unnecessary mutation.' }
    & $nativeModule { $script:FixtureListeners = @(@{ address = '127.0.0.1'; pid = 99999 }) }
    foreach ($action in @('up', 'check', 'down')) { Assert-NativeThrows { Invoke-KubixLabWindowsModel -Action $action } "unmarked process port adopted during $action" }
    if (@(& $nativeModule { $script:NativeOperations }).Count -ne 0) { throw 'An unowned service was modified.' }
} finally {
    Remove-Module Kubix.LabModel.Windows
    Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.Windows.psm1') -Force
}
$nativeModule = Get-Module Kubix.LabModel.Windows
# A real CIM snapshot truncates timestamp precision. Keep the exact Get-Process
# identity for the post-stop check, and reject descendants outside this runtime.
& $nativeModule {
    param($Marker)
    $script:ChildIdentity = @{
        pid = 12346; executable = $Marker.executable; started_at = '2026-11-13T19:30:08.1920915Z'; command_line = 'runner'
    }
    $script:ChildSnapshot = [pscustomobject]@{
        ProcessId = 12346; ParentProcessId = $Marker.pid; ExecutablePath = $Marker.executable
        CreationDate = [DateTime]::Parse('2026-11-13T19:30:08.1920910Z', $null, [Globalization.DateTimeStyles]::RoundtripKind)
    }
    function script:Get-CimInstance { @($script:ChildSnapshot) }
    function script:Get-NativeProcess { param($ServerProcessId) $script:ChildIdentity }
} (New-NativeMarkerFixture)
try {
    $children = @(Invoke-NativeTestFunction Get-NativeDescendants @((New-NativeMarkerFixture)))
    if ($children.Count -ne 1 -or $children[0].started_at -cne '2026-11-13T19:30:08.1920915Z') {
        throw 'Descendant identity lost Get-Process timestamp precision.'
    }
    & $nativeModule {
        $script:ChildSnapshot.ExecutablePath = Join-Path ([Environment]::SystemDirectory) 'conhost.exe'
        $script:ChildIdentity.executable = $script:ChildSnapshot.ExecutablePath
    }
    $consoleChildren = @(Invoke-NativeTestFunction Get-NativeDescendants @((New-NativeMarkerFixture)))
    if ($consoleChildren.Count -ne 1) { throw 'The owned hidden console host was not recognized.' }
    & $nativeModule {
        $script:ChildSnapshot.ExecutablePath = Join-Path ([Environment]::SystemDirectory) 'cmd.exe'
        $script:ChildIdentity.executable = $script:ChildSnapshot.ExecutablePath
    }
    Assert-NativeThrows { Invoke-NativeTestFunction Get-NativeDescendants @((New-NativeMarkerFixture)) } 'arbitrary system executable descendant'
    & $nativeModule { $script:ChildSnapshot.ExecutablePath = 'C:\unrelated\application.exe' }
    Assert-NativeThrows { Invoke-NativeTestFunction Get-NativeDescendants @((New-NativeMarkerFixture)) } 'unrelated descendant process-tree shutdown'
} finally {
    Remove-Module Kubix.LabModel.Windows
    Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.Windows.psm1') -Force
}
$nativeModule = Get-Module Kubix.LabModel.Windows
# Startup must isolate the child without permanently altering this shell or user
# environment. Force a launch failure and verify finally restores every setting.
$originalCloud = [Environment]::GetEnvironmentVariable('OLLAMA_NO_CLOUD', 'Process')
$originalProfile = [Environment]::GetEnvironmentVariable('USERPROFILE', 'Process')
$originalProxy = [Environment]::GetEnvironmentVariable('ALL_PROXY', 'Process')
try {
    [Environment]::SetEnvironmentVariable('OLLAMA_NO_CLOUD', 'test-original', 'Process')
    [Environment]::SetEnvironmentVariable('ALL_PROXY', 'http://test.invalid', 'Process')
    & $nativeModule {
        $script:LaunchBoundaryObserved = $false
        function script:New-Item { }
        function script:Start-Process {
            param($FilePath, $ArgumentList, $WorkingDirectory, $WindowStyle, [switch]$PassThru, $RedirectStandardOutput, $RedirectStandardError)
            if ($WindowStyle -ne 'Hidden' -or $env:OLLAMA_NO_CLOUD -ne '1' -or $env:OLLAMA_HOST -ne '127.0.0.1:11436' -or
                $env:OLLAMA_KEEP_ALIVE -ne '5m' -or $env:ALL_PROXY -or ($ArgumentList -join ' ') -ne 'serve') {
                throw 'Child isolation was not applied at the launch boundary.'
            }
            $null = Assert-NativeLabPath $env:USERPROFILE
            $script:LaunchBoundaryObserved = $true
            throw 'Intentional mock launch failure.'
        }
    }
    Assert-NativeThrows { Invoke-NativeTestFunction Start-NativeServer @($configuration, @{ executable_sha256 = ('a' * 64) }) } 'mock launch failure'
    if (-not (& $nativeModule { $script:LaunchBoundaryObserved })) { throw 'The isolated launch boundary was not exercised.' }
    if ($env:OLLAMA_NO_CLOUD -ne 'test-original' -or $env:USERPROFILE -ne $originalProfile -or $env:ALL_PROXY -ne 'http://test.invalid') {
        throw 'Native launch leaked environment changes into the parent shell.'
    }
} finally {
    [Environment]::SetEnvironmentVariable('OLLAMA_NO_CLOUD', $originalCloud, 'Process')
    [Environment]::SetEnvironmentVariable('USERPROFILE', $originalProfile, 'Process')
    [Environment]::SetEnvironmentVariable('ALL_PROXY', $originalProxy, 'Process')
    Remove-Module Kubix.LabModel.Windows
    Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.Windows.psm1') -Force
}
Write-Host 'Native model guards passed (no network, GPU inference, or process launches).'
