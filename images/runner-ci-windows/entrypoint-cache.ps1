$ErrorActionPreference = "Stop"
$Cache = "C:\ephemerd-ci"
$Work = if ($env:GITHUB_WORKSPACE) { $env:GITHUB_WORKSPACE } else { Get-Location }

Write-Host "Restoring cached build deps from $Cache..."

foreach ($dir in @("pkg\runner\embed", "bin", "pkg\vm\embed")) {
    New-Item -ItemType Directory -Force -Path "$Work\$dir" | Out-Null
}

Copy-Item "$Cache\pkg\runner\embed\*" "$Work\pkg\runner\embed\" -ErrorAction SilentlyContinue
Copy-Item "$Cache\bin\*" "$Work\bin\" -ErrorAction SilentlyContinue

if (-not (Test-Path "$Work\pkg\vm\embed\ephemerd-linux")) {
    New-Item -ItemType File -Path "$Work\pkg\vm\embed\ephemerd-linux" | Out-Null
}

# Ensure tools baked into the image are on PATH for subsequent steps.
# Hyper-V isolated containers don't always propagate the image's ENV PATH
# through containerd → cmd.exe → runner → PowerShell, so we write to
# GITHUB_PATH which the runner reads for every step.
#
# C:\go\bin is GOBIN (mage, docker.exe, anything a workflow `go install`s).
# The Go toolchain itself lives in the runner tool cache — see the Dockerfile —
# so its bin directory is versioned and has to be discovered rather than
# hardcoded. Highest version wins; a workflow that runs actions/setup-go later
# prepends its own choice and overrides this.
if ($env:GITHUB_PATH) {
    if (Test-Path "C:\go\bin") {
        Add-Content -Path $env:GITHUB_PATH -Value "C:\go\bin"
        Write-Host "Added C:\go\bin to GITHUB_PATH"
    }

    $ToolCache = if ($env:RUNNER_TOOL_CACHE) { $env:RUNNER_TOOL_CACHE } else { "C:\hostedtoolcache" }
    $GoBin = Get-ChildItem "$ToolCache\go" -Directory -ErrorAction SilentlyContinue |
        Sort-Object { try { [version]$_.Name } catch { [version]"0.0.0" } } -Descending |
        ForEach-Object { Join-Path $_.FullName "x64\bin" } |
        Where-Object { Test-Path $_ } |
        Select-Object -First 1
    if ($GoBin) {
        Add-Content -Path $env:GITHUB_PATH -Value $GoBin
        Write-Host "Added $GoBin to GITHUB_PATH"
    }
}

Write-Host "Build deps restored."
