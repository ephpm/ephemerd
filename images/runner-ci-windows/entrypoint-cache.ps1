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

Write-Host "Build deps restored."
