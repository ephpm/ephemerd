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
if ($env:GITHUB_PATH) {
    foreach ($dir in @("C:\go\bin", "C:\Users\ContainerUser\go\bin")) {
        if (Test-Path $dir) {
            Add-Content -Path $env:GITHUB_PATH -Value $dir
            Write-Host "Added $dir to GITHUB_PATH"
        }
    }
}

Write-Host "Build deps restored."
