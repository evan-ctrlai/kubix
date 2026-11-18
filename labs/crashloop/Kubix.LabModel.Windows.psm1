#Requires -Version 7.0
Set-StrictMode -Version Latest
Import-Module (Join-Path $PSScriptRoot 'Kubix.LabModel.psm1')
$script:NativeRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$script:NativeOwner = 'crashloop-windows-model-v1'

function Assert-NativeLabPath {
    param([string]$Path)
    $full = [IO.Path]::GetFullPath($Path)
    $local = [IO.Path]::GetFullPath((Join-Path $script:NativeRoot '.local'))
    if (-not $full.StartsWith($local + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Native model files must remain inside this checkout .local directory.'
    }
    # A link or junction could turn a lexical workspace path into an outside write.
    $cursor = $full
    while ($cursor -and $cursor.Length -ge $local.Length) {
        if (Test-Path -LiteralPath $cursor) {
            if ((Get-Item -LiteralPath $cursor -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) {
                throw 'Native model paths may not contain links or junctions.'
            }
        }
        $cursor = [IO.Path]::GetDirectoryName($cursor)
    }
    return $full
}

function Get-KubixLabWindowsModelConfiguration {
    $c = Get-KubixLabModelConfiguration
    $pin = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'model-windows-lock.json') -Raw | ConvertFrom-Json -AsHashtable
    if ($pin.server_version -ne $c.server_version -or $pin.port -ne 11436 -or $c.server_version -notmatch '^\d+\.\d+\.\d+$') {
        throw 'The native runtime and shared model locks disagree.'
    }
    foreach ($asset in @($pin.main, $pin.rocm)) {
        if ($asset.sha256 -notmatch '^[a-f0-9]{64}$' -or $asset.name -notmatch '^ollama-windows-amd64(-rocm)?\.zip$') {
            throw 'The native archive is not pinned to a safe asset name and SHA256.'
        }
    }
    $c.native_pin = $pin
    $c.port = $pin.port
    $c.endpoint = "http://127.0.0.1:$($c.port)"
    $c.state_directory = Assert-NativeLabPath (Join-Path $script:NativeRoot '.local/lab')
    $c.runtime = Assert-NativeLabPath (Join-Path $script:NativeRoot ".local/tools/ollama/$($c.server_version)")
    $c.executable = Join-Path $c.runtime 'ollama.exe'
    $c.install_marker = Join-Path $c.runtime 'install.json'
    $c.state_file = Join-Path $c.state_directory 'model-windows-state.json'
    $c.model_directory = Join-Path $c.state_directory 'ollama-models'
    $c.stdout = Join-Path $c.state_directory 'model-windows.stdout.log'
    $c.stderr = Join-Path $c.state_directory 'model-windows.stderr.log'
    $c.profile = Join-Path $c.state_directory 'ollama-profile'
    $c.temporary = Join-Path $c.state_directory 'ollama-tmp'
    foreach ($path in @($c.executable, $c.install_marker, $c.state_file, $c.model_directory, $c.stdout, $c.stderr,
            $c.profile, $c.temporary, (Join-Path $c.profile 'AppData/Local'), (Join-Path $c.profile 'AppData/Roaming'))) {
        $null = Assert-NativeLabPath $path
    }
    return $c
}

function Get-NativeEnvironment {
    param([hashtable]$Configuration, [string]$VulkanDevice = '')
    $c = $Configuration
    $settings = @{
        OLLAMA_HOST = "127.0.0.1:$($c.port)"; OLLAMA_NO_CLOUD = '1'; OLLAMA_MODELS = $c.model_directory
        OLLAMA_CONTEXT_LENGTH = [string]$c.context_length; OLLAMA_NUM_PARALLEL = '1'; OLLAMA_MAX_LOADED_MODELS = '1'
        OLLAMA_KEEP_ALIVE = '5m'; OLLAMA_VULKAN = '1'; OLLAMA_TMPDIR = $c.temporary
        USERPROFILE = $c.profile; LOCALAPPDATA = (Join-Path $c.profile 'AppData/Local'); APPDATA = (Join-Path $c.profile 'AppData/Roaming')
        TEMP = $c.temporary; TMP = $c.temporary
    }
    if ($VulkanDevice) {
        if ($VulkanDevice -notmatch '^[0-9]+$') { throw 'The Vulkan selector must be a numeric device index.' }
        $settings.GGML_VK_VISIBLE_DEVICES = $VulkanDevice
    }
    return $settings
}

function Read-NativeMarker {
    param([string]$Path)
    $null = Assert-NativeLabPath $Path
    if (-not (Test-Path -LiteralPath $Path)) { return $null }
    if ((Get-Item -LiteralPath $Path).Length -gt 32768) { throw 'The native ownership marker is unexpectedly large.' }
    return Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable
}

function ConvertTo-NativeTimestamp {
    param($Value)
    # ConvertFrom-Json can materialize ISO timestamps as DateTime. Passing that
    # value to Parse would first format it using local culture, losing both UTC
    # and fractional seconds, so preserve the typed value when present.
    if ($Value -is [DateTimeOffset]) { return $Value.ToUniversalTime() }
    if ($Value -is [DateTime]) { return ([DateTimeOffset]$Value).ToUniversalTime() }
    return [DateTimeOffset]::Parse([string]$Value, [Globalization.CultureInfo]::InvariantCulture).ToUniversalTime()
}

function Assert-NativeMarker {
    param([hashtable]$Marker, [hashtable]$Configuration, [switch]$Installation, [switch]$OwnershipOnly)
    $c = $Configuration
    if (-not $Marker -or $Marker.owner -ne $script:NativeOwner -or $Marker.workspace -ne $c.workspace -or
        $Marker.executable -ne $c.executable -or $Marker.executable_sha256 -notmatch '^[a-f0-9]{64}$') {
        throw 'The native model is not owned by this checkout. It will not be reused or stopped.'
    }
    if (-not $OwnershipOnly -and $Marker.server_version -ne $c.server_version) { throw 'The native runtime version differs from the lock.' }
    if ($Installation) {
        if ($Marker.main_sha256 -ne $c.native_pin.main.sha256 -or $Marker.rocm_sha256 -ne $c.native_pin.rocm.sha256) {
            throw 'The installed native archives differ from their pinned hashes.'
        }
    } else {
        if ($Marker.endpoint -ne $c.endpoint -or $Marker.model_directory -ne $c.model_directory -or
            $Marker.stdout -ne $c.stdout -or $Marker.stderr -ne $c.stderr -or [long]$Marker.pid -le 0) {
            throw 'The native model marker has unexpected process, endpoint, or file paths.'
        }
        $null = ConvertTo-NativeTimestamp $Marker.started_at
        if (-not $OwnershipOnly) {
            $expected = Get-NativeEnvironment $c
            foreach ($name in $expected.Keys) {
                if ($Marker.environment[$name] -ne $expected[$name]) { throw "Native model setting $name differs from the isolated lab configuration." }
            }
            foreach ($name in $Marker.environment.Keys) {
                if ($expected.ContainsKey($name)) { continue }
                if ($name -eq 'GGML_VK_VISIBLE_DEVICES' -and $Marker.environment[$name] -match '^[0-9]+$') { continue }
                throw 'The native model marker contains an unexpected environment setting.'
            }
        }
    }
}

function Assert-NativeExecutable {
    param([hashtable]$Marker, [hashtable]$Configuration)
    $null = Assert-NativeLabPath $Configuration.executable
    if (-not (Test-Path -LiteralPath $Configuration.executable -PathType Leaf) -or
        (Get-FileHash -LiteralPath $Configuration.executable -Algorithm SHA256).Hash.ToLowerInvariant() -ne $Marker.executable_sha256) {
        throw 'The native executable has changed since installation or launch.'
    }
}

function Resolve-NativeZipEntry {
    param([string]$Destination, [string]$Name, [long]$ExternalAttributes = 0)
    # Reject Windows absolute/ADS paths, Unix symlinks, and traversal before extraction.
    if (-not $Name -or $Name -match '[:\x00-\x1f]' -or $Name.StartsWith('/') -or $Name.StartsWith('\') -or
        ($Name -split '[/\\]') -contains '..' -or (($ExternalAttributes -shr 16) -band 61440) -eq 40960) {
        throw 'The pinned archive contains an unsafe entry.'
    }
    $base = Assert-NativeLabPath $Destination
    $target = Assert-NativeLabPath (Join-Path $base $Name)
    if (-not $target.StartsWith($base + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'The archive entry escapes its extraction directory.'
    }
    return $target
}

function Install-NativeRuntime {
    param([hashtable]$Configuration)
    $c = $Configuration
    $existing = Read-NativeMarker $c.install_marker
    if ($existing) { Assert-NativeMarker $existing $c -Installation; Assert-NativeExecutable $existing $c; return $existing }
    if (Test-Path -LiteralPath $c.runtime) { throw 'An unmarked runtime directory exists. Refusing to overwrite it.' }
    $downloadDirectory = Assert-NativeLabPath (Join-Path $script:NativeRoot '.local/downloads')
    $null = New-Item -ItemType Directory -Path $downloadDirectory -Force
    $stage = Assert-NativeLabPath ($c.runtime + '.staging-' + [Guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $stage
    foreach ($asset in @($c.native_pin.main, $c.native_pin.rocm)) {
        $archive = Assert-NativeLabPath (Join-Path $downloadDirectory ($c.server_version + '-' + $asset.name))
        if (-not (Test-Path -LiteralPath $archive)) {
            Write-Host "Downloading pinned native Ollama asset $($asset.name)..."
            $partial = Assert-NativeLabPath ($archive + '.partial-' + [Guid]::NewGuid().ToString('N'))
            Invoke-WebRequest -Uri "https://github.com/ollama/ollama/releases/download/v$($c.server_version)/$($asset.name)" -OutFile $partial -TimeoutSec 900 -MaximumRedirection 5
            if ((Get-FileHash -LiteralPath $partial -Algorithm SHA256).Hash.ToLowerInvariant() -ne $asset.sha256) { throw 'Downloaded native archive failed SHA256 verification.' }
            Move-Item -LiteralPath $partial -Destination $archive
        }
        if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $asset.sha256) { throw 'Cached native archive failed SHA256 verification.' }
        $zip = [IO.Compression.ZipFile]::OpenRead($archive)
        try {
            foreach ($entry in $zip.Entries) {
                $target = Resolve-NativeZipEntry $stage $entry.FullName $entry.ExternalAttributes
                if ($entry.FullName.EndsWith('/') -or $entry.FullName.EndsWith('\')) { $null = New-Item -ItemType Directory -Path $target -Force; continue }
                if (Test-Path -LiteralPath $target) { throw 'Native archives contain overlapping files.' }
                $null = New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($target)) -Force
                [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $target, $false)
            }
        } finally { $zip.Dispose() }
    }
    if (-not (Test-Path -LiteralPath (Join-Path $stage 'ollama.exe') -PathType Leaf)) { throw 'The pinned native archive lacks ollama.exe.' }
    $null = Assert-NativeLabPath $stage
    $null = Assert-NativeLabPath $c.runtime
    Move-Item -LiteralPath $stage -Destination $c.runtime
    $marker = @{
        owner = $script:NativeOwner; workspace = $c.workspace; server_version = $c.server_version
        main_sha256 = $c.native_pin.main.sha256; rocm_sha256 = $c.native_pin.rocm.sha256
        executable = $c.executable; executable_sha256 = (Get-FileHash -LiteralPath $c.executable -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    $marker | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $c.install_marker -Encoding utf8
    return $marker
}

function Get-NativeProcess {
    param([int]$ServerProcessId)
    $process = Get-Process -Id $ServerProcessId -ErrorAction SilentlyContinue
    if (-not $process) { return $null }
    $details = Get-CimInstance Win32_Process -Filter "ProcessId = $ServerProcessId" -ErrorAction Stop
    if (-not $details) { throw 'Cannot verify the native process command line.' }
    return @{ pid = $process.Id; executable = $process.Path; started_at = $process.StartTime.ToUniversalTime().ToString('o'); command_line = $details.CommandLine }
}

function Assert-NativeProcess {
    param([hashtable]$Process, [hashtable]$Marker)
    $quotedPath = [regex]::Escape($Marker.executable)
    if (-not $Process -or $Process.pid -ne $Marker.pid -or $Process.executable -ne $Marker.executable -or
        (ConvertTo-NativeTimestamp $Process.started_at) -ne (ConvertTo-NativeTimestamp $Marker.started_at) -or
        $Process.command_line -notmatch ('^\s*(?:"' + $quotedPath + '"|' + $quotedPath + ')\s+serve\s*$')) {
        throw 'The recorded PID does not identify the original native model server. Refusing to reuse or stop it.'
    }
}

function Get-NativeListeners {
    param([int]$Port)
    return @(Get-NetTCPConnection -State Listen -ErrorAction Stop | Where-Object LocalPort -EQ $Port | ForEach-Object {
        @{ address = $_.LocalAddress; pid = $_.OwningProcess }
    })
}

function Assert-NativeListeners {
    param([array]$Listeners, [int]$ServerProcessId = 0, [switch]$RequireListening)
    if ($RequireListening -and $Listeners.Count -eq 0) { throw 'The native model server is not listening.' }
    foreach ($listener in $Listeners) {
        if ($ServerProcessId -eq 0 -or $listener.pid -ne $ServerProcessId -or $listener.address -ne '127.0.0.1') {
            throw 'The native model port is occupied by an unowned process or exposed beyond localhost.'
        }
    }
}

function Invoke-NativeRequest {
    param([hashtable]$Configuration, [string]$Path, [hashtable]$Body, [int]$TimeoutSeconds = 15)
    if ($Configuration.endpoint -cne 'http://127.0.0.1:11436' -or $Path -notin @('/api/version', '/api/tags', '/api/pull', '/api/ps', '/v1/chat/completions')) {
        throw 'Native model requests must target the fixed localhost API.'
    }
    $handler = [Net.Http.HttpClientHandler]::new()
    $handler.UseProxy = $false; $handler.AllowAutoRedirect = $false
    $client = [Net.Http.HttpClient]::new($handler)
    $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSeconds)
    $request = [Net.Http.HttpRequestMessage]::new($(if ($Body) { [Net.Http.HttpMethod]::Post } else { [Net.Http.HttpMethod]::Get }), $Configuration.endpoint + $Path)
    $deadline = [Threading.CancellationTokenSource]::new([TimeSpan]::FromSeconds($TimeoutSeconds))
    try {
        if ($Body) { $request.Content = [Net.Http.StringContent]::new(($Body | ConvertTo-Json -Depth 20 -Compress), [Text.Encoding]::UTF8, 'application/json') }
        $response = $client.SendAsync($request, [Net.Http.HttpCompletionOption]::ResponseHeadersRead, $deadline.Token).GetAwaiter().GetResult()
        try {
            if (-not $response.IsSuccessStatusCode) { throw "Native model API returned HTTP $([int]$response.StatusCode)." }
            if ($response.Content.Headers.ContentLength -gt 2097152) { throw 'Native model response exceeds the 2 MiB limit.' }
            $stream = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
            $bytes = [IO.MemoryStream]::new()
            try {
                $buffer = [byte[]]::new(8192)
                while (($count = $stream.ReadAsync($buffer, 0, $buffer.Length, $deadline.Token).GetAwaiter().GetResult()) -gt 0) {
                    if ($bytes.Length + $count -gt 2097152) { throw 'Native model response exceeds the 2 MiB limit.' }
                    $bytes.Write($buffer, 0, $count)
                }
                return [Text.Encoding]::UTF8.GetString($bytes.ToArray()) | ConvertFrom-Json -AsHashtable
            } finally { $bytes.Dispose(); $stream.Dispose() }
        } finally { $response.Dispose() }
    } finally { $deadline.Dispose(); $request.Dispose(); $client.Dispose(); $handler.Dispose() }
}

function Start-NativeServer {
    param([hashtable]$Configuration, [hashtable]$Installation, [string]$VulkanDevice = '')
    $c = $Configuration
    $settings = Get-NativeEnvironment $c $VulkanDevice
    foreach ($directory in @($c.state_directory, $c.model_directory, $c.profile, $c.temporary, $settings.APPDATA, $settings.LOCALAPPDATA)) {
        $null = Assert-NativeLabPath $directory
        $null = New-Item -ItemType Directory -Path $directory -Force
    }
    foreach ($path in @($c.state_file, $c.stdout, $c.stderr)) { $null = Assert-NativeLabPath $path }
    # Environment changes are process-scoped and restored even if launch fails.
    # Clear inherited GPU/model/proxy overrides before constructing the child env.
    $saved = @{}
    $names = @([Environment]::GetEnvironmentVariables('Process').Keys | Where-Object { $_ -match '^(OLLAMA_|GGML_|CUDA_|HIP_|ROCR_|HSA_)|^(GPU_DEVICE_ORDINAL|HTTP_PROXY|HTTPS_PROXY|ALL_PROXY|NO_PROXY)$' }) + @($settings.Keys)
    try {
        foreach ($name in ($names | Select-Object -Unique)) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process'); [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
        foreach ($name in $settings.Keys) { [Environment]::SetEnvironmentVariable($name, $settings[$name], 'Process') }
        $process = Start-Process -FilePath $c.executable -ArgumentList @('serve') -WorkingDirectory $c.runtime -WindowStyle Hidden -PassThru -RedirectStandardOutput $c.stdout -RedirectStandardError $c.stderr
    } finally {
        foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') }
    }
    $marker = @{
        owner = $script:NativeOwner; workspace = $c.workspace; pid = $process.Id; executable = $c.executable
        executable_sha256 = $Installation.executable_sha256; started_at = $process.StartTime.ToUniversalTime().ToString('o')
        endpoint = $c.endpoint; model_directory = $c.model_directory; server_version = $c.server_version
        environment = $settings; stdout = $c.stdout; stderr = $c.stderr
    }
    $marker | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $c.state_file -Encoding utf8
    return $marker
}

function Get-NativePinnedModel {
    param([hashtable]$Configuration, [switch]$PullIfMissing)
    $tags = Invoke-NativeRequest $Configuration '/api/tags'
    $matches = @($tags.models | Where-Object { $_.name -eq $Configuration.model })
    if ($matches.Count -eq 0 -and $PullIfMissing) {
        Write-Host "Downloading the pinned $($Configuration.model) model into this checkout..."
        $null = Invoke-NativeRequest $Configuration '/api/pull' @{ name = $Configuration.model; stream = $false } 900
        $tags = Invoke-NativeRequest $Configuration '/api/tags'
        $matches = @($tags.models | Where-Object { $_.name -eq $Configuration.model })
    }
    if ($matches.Count -ne 1) { throw 'The locked model is missing. Run native model up first.' }
    Assert-LabModelDigest $matches[0].digest $Configuration
    return $matches[0]
}

function Get-NativeGPUProof {
    param([hashtable]$Configuration, [hashtable]$Running)
    $models = @($Running.models | Where-Object { $_.name -eq $Configuration.model })
    if ($models.Count -ne 1) { throw 'The probe did not leave the pinned model loaded.' }
    $model = $models[0]
    Assert-LabModelDigest $model.digest $Configuration
    if ([long]$model.size_vram -le 0) { throw 'The model is using CPU only. Native GPU validation did not pass.' }
    if ([long]$model.context_length -ne $Configuration.context_length) { throw 'The loaded native model uses an unexpected context length.' }
    return @{ name = $model.name; digest = $model.digest; size_bytes = $model.size; gpu_allocated_bytes = $model.size_vram; context_length = $model.context_length }
}

function Get-NativeGPUDetails {
    param([hashtable]$Configuration)
    $hardware = @(Get-CimInstance Win32_VideoController -ErrorAction Stop | ForEach-Object { @{ name = $_.Name; driver_version = $_.DriverVersion } })
    $lines = @()
    if (Test-Path -LiteralPath $Configuration.stderr) {
        $null = Assert-NativeLabPath $Configuration.stderr
        $lines = @(Get-Content -LiteralPath $Configuration.stderr -Tail 150 | Where-Object { $_ -match '(?i)offload|inference compute|library=|ROCm|Vulkan|CUDA|AMD Radeon|NVIDIA|Intel.*(Arc|GPU)' } | Select-Object -Last 20 | ForEach-Object {
            $clean = ([string]$_) -replace '[\x00-\x1f\x7f-\x9f]', ''
            $clean.Substring(0, [Math]::Min(500, $clean.Length))
        })
    }
    return @{ detected_adapters = $hardware; runtime_log_evidence = $lines }
}

function Test-NativeModel {
    param([hashtable]$Configuration)
    $c = $Configuration
    $version = Invoke-NativeRequest $c '/api/version'
    if ($version.version -ne $c.server_version) { throw 'The native API runtime version differs from the lock.' }
    $model = Get-NativePinnedModel $c
    $watch = [Diagnostics.Stopwatch]::StartNew()
    $answer = Invoke-NativeRequest $c '/v1/chat/completions' @{
        model = $c.model; messages = @(@{ role = 'user'; content = 'Reply with exactly KUBIX_LAB_READY and no other text.' })
        stream = $false; temperature = 0; max_tokens = 64
    } 300
    $watch.Stop()
    if (@($answer.choices).Count -ne 1 -or $answer.choices[0].message.role -ne 'assistant' -or $answer.choices[0].message.content.Trim() -ne 'KUBIX_LAB_READY') {
        throw 'The native model did not complete the local inference probe.'
    }
    $loaded = Get-NativeGPUProof $c (Invoke-NativeRequest $c '/api/ps')
    $details = Get-NativeGPUDetails $c
    $report = [ordered]@{
        checked_at = [DateTime]::UtcNow.ToString('o'); endpoint = $c.endpoint; model = $c.model; model_digest = $model.digest
        server_version = $version.version; context_length = $c.context_length; cloud_disabled = $true; runtime = 'windows-native'
        probe_duration_ms = [Math]::Round($watch.Elapsed.TotalMilliseconds); probe_passed = $true; gpu_offload_proven = $true
        diagnostic_quality_validated = $false; loaded_model = $loaded; detected_adapters = $details.detected_adapters; runtime_log_evidence = $details.runtime_log_evidence
    }
    $reportPath = Assert-NativeLabPath (Join-Path $c.state_directory 'model-windows-validation.json')
    $report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $reportPath -Encoding utf8
    Write-Host "Native GPU probe passed in $($report.probe_duration_ms) ms; allocated $($loaded.gpu_allocated_bytes) GPU bytes. Report: $reportPath"
    return $report
}

function Stop-NativeServer {
    param([hashtable]$Marker)
    # Kill only the verified server tree, including its GPU runner. The root
    # process handle is checked again at the termination boundary to reject PID reuse.
    Assert-NativeProcess (Get-NativeProcess ([int]$Marker.pid)) $Marker
    $descendants = @(Get-NativeDescendants $Marker)
    Invoke-NativeTreeStop $Marker
    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    do {
        $remaining = @($descendants | Where-Object {
            $current = Get-NativeProcess ([int]$_.pid)
            $current -and $current.executable -eq $_.executable -and
                (ConvertTo-NativeTimestamp $current.started_at) -eq (ConvertTo-NativeTimestamp $_.started_at)
        })
        if ($remaining.Count -eq 0) { return }
        Start-Sleep -Milliseconds 100
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'The owned model server stopped, but a recorded runner has not exited. Retaining the state marker for inspection.'
}

function Get-NativeDescendants {
    param([hashtable]$Marker)
    $all = @(Get-CimInstance Win32_Process -ErrorAction Stop)
    $pending = [Collections.Generic.Queue[long]]::new()
    $pending.Enqueue([long]$Marker.pid)
    $seen = [Collections.Generic.HashSet[long]]::new()
    $null = $seen.Add([long]$Marker.pid)
    $runtime = [IO.Path]::GetDirectoryName($Marker.executable) + [IO.Path]::DirectorySeparatorChar
    $consoleHost = Join-Path ([Environment]::SystemDirectory) 'conhost.exe'
    while ($pending.Count -gt 0) {
        $parentId = $pending.Dequeue()
        foreach ($child in @($all | Where-Object ParentProcessId -EQ $parentId)) {
            if (-not $seen.Add([long]$child.ProcessId)) { throw 'Unexpected cycle in native process ancestry.' }
            $identity = Get-NativeProcess ([int]$child.ProcessId)
            if (-not $identity) { continue }
            $allowedExecutable = $child.ExecutablePath -and
                ($child.ExecutablePath.StartsWith($runtime, [StringComparison]::OrdinalIgnoreCase) -or $child.ExecutablePath -eq $consoleHost)
            if (-not $allowedExecutable -or
                $identity.executable -ne $child.ExecutablePath -or
                (ConvertTo-NativeTimestamp $identity.started_at) -lt (ConvertTo-NativeTimestamp $Marker.started_at) -or
                [Math]::Abs(($child.CreationDate.ToUniversalTime() - (ConvertTo-NativeTimestamp $identity.started_at).UtcDateTime).TotalMilliseconds) -gt 1) {
                throw 'The native server has an unexpected descendant; refusing process-tree termination.'
            }
            # CIM timestamps can truncate sub-microsecond precision. Use the same
            # Get-Process identity source here and in the post-termination check.
            @{ pid = $identity.pid; executable = $identity.executable; started_at = $identity.started_at }
            $pending.Enqueue([long]$child.ProcessId)
        }
    }
}

function Invoke-NativeTreeStop {
    param([hashtable]$Marker)
    $handle = Get-Process -Id ([int]$Marker.pid) -ErrorAction Stop
    try {
        if ($handle.Path -ne $Marker.executable -or $handle.StartTime.ToUniversalTime() -ne (ConvertTo-NativeTimestamp $Marker.started_at).UtcDateTime) {
            throw 'The native PID changed before process-tree termination.'
        }
        $handle.Kill($true)
        if (-not $handle.WaitForExit(10000)) { throw 'The owned native model server did not exit within 10 seconds.' }
    } finally { $handle.Dispose() }
}

function Invoke-KubixLabWindowsModel {
    [CmdletBinding()]
    param([ValidateSet('up', 'check', 'down')][string]$Action = 'check', [string]$StateDirectory = '', [string]$VulkanDevice = '')
    if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne [Runtime.InteropServices.Architecture]::X64) {
        throw 'The portable GPU runtime requires Windows x64.'
    }
    $c = Get-KubixLabWindowsModelConfiguration
    if ($StateDirectory -and (Assert-NativeLabPath $StateDirectory) -ne $c.state_directory) { throw 'Native model state uses the fixed checkout .local/lab directory.' }
    if ($VulkanDevice -and $VulkanDevice -notmatch '^[0-9]+$') { throw 'The Vulkan selector must be a numeric device index.' }
    $marker = Read-NativeMarker $c.state_file
    if ($marker) {
        Assert-NativeMarker $marker $c -OwnershipOnly:($Action -eq 'down')
        Assert-NativeExecutable $marker $c
        $process = Get-NativeProcess ([int]$marker.pid)
        if ($process) { Assert-NativeProcess $process $marker }
        Assert-NativeListeners @(Get-NativeListeners $c.port) $(if ($process) { [int]$marker.pid } else { 0 })
        if (-not $process) {
            if (@(Get-NativeDescendants $marker).Count -gt 0) {
                throw 'The recorded server is gone but descendants remain. Retaining its state marker for inspection; no orphan processes were stopped.'
            }
            if ($Action -eq 'check') { throw 'The recorded native model server is no longer running.' }
            Remove-Item -LiteralPath $c.state_file
            $marker = $null
        }
    } else { Assert-NativeListeners @(Get-NativeListeners $c.port) }
    if ($Action -eq 'down') {
        if ($marker) {
            Stop-NativeServer $marker
            Assert-NativeListeners @(Get-NativeListeners $c.port)
            Remove-Item -LiteralPath $c.state_file
        }
        Write-Host 'Owned native model server and GPU runners stopped; localhost port is clear. Downloaded binaries and model weights are retained.'
        return
    }
    if ($Action -eq 'check' -and -not $marker) { throw 'No owned native model server exists. Run native model up first.' }
    if ($Action -eq 'check') {
        $installation = Read-NativeMarker $c.install_marker
        Assert-NativeMarker $installation $c -Installation
        Assert-NativeExecutable $installation $c
        if ($marker.executable_sha256 -ne $installation.executable_sha256) { throw 'The launched native process differs from the pinned installation.' }
    }
    if ($Action -eq 'up') {
        $installation = Install-NativeRuntime $c
        if ($marker) {
            if ($marker.executable_sha256 -ne $installation.executable_sha256) { throw 'The launched native process differs from the pinned installation.' }
            if ($VulkanDevice -and $marker.environment['GGML_VK_VISIBLE_DEVICES'] -ne $VulkanDevice) { throw 'Changing the GPU selector requires native model down then up.' }
        } else {
            Assert-NativeListeners @(Get-NativeListeners $c.port)
            $marker = Start-NativeServer $c $installation $VulkanDevice
        }
        $deadline = [DateTime]::UtcNow.AddSeconds(60)
        while ($true) {
            Assert-NativeProcess (Get-NativeProcess ([int]$marker.pid)) $marker
            $listeners = @(Get-NativeListeners $c.port)
            Assert-NativeListeners $listeners ([int]$marker.pid)
            if ($listeners.Count -gt 0) { break }
            if ([DateTime]::UtcNow -ge $deadline) { throw 'The owned native model did not listen within 60 seconds; see its local stderr log.' }
            Start-Sleep -Milliseconds 500
        }
        $version = Invoke-NativeRequest $c '/api/version'
        if ($version.version -ne $c.server_version) { throw 'The native API runtime version differs from the lock.' }
        $null = Get-NativePinnedModel $c -PullIfMissing
    }
    Assert-NativeListeners @(Get-NativeListeners $c.port) ([int]$marker.pid) -RequireListening
    $null = Test-NativeModel $c
}

Export-ModuleMember -Function Get-KubixLabWindowsModelConfiguration, Invoke-KubixLabWindowsModel
