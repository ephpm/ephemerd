# escape=`
# CI runner image for Windows — build deps pre-cached.
# Must be built on a Windows host.
#
# Parser directive `# escape=`` ` must be the very first line — before any
# regular comments — for both the legacy and buildkit Windows parsers to
# honor it. Without it, backtick line continuations get parsed as unknown
# instructions and Windows paths (C:\...) eat the next character.

FROM mcr.microsoft.com/windows/servercore:ltsc2025

ARG RUNNER_VERSION=2.333.1
ARG GOLANGCI_LINT_VERSION=2.11.4
ARG GO_VERSION=1.26.1
ARG DOCKER_CLI_VERSION=27.3.1

# $ProgressPreference = 'SilentlyContinue' is required for Invoke-WebRequest
# in non-interactive PowerShell. Without it the cmdlet tries to render its
# progress bar to a non-existent console and hangs (or runs ~50× slower) on
# Windows containers — the symptom is RUN steps that download large files
# stalling indefinitely. See https://github.com/PowerShell/PowerShell/issues/2138
SHELL ["powershell", "-Command", "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]

RUN Invoke-WebRequest -Uri "https://go.dev/dl/go${env:GO_VERSION}.windows-amd64.zip" -OutFile go.zip; `
    Expand-Archive go.zip -DestinationPath C:\; `
    Remove-Item go.zip

# Set PATH explicitly. ${PATH} substitution in Windows Dockerfiles is
# unreliable across SHELL changes; the GitHub Actions runner spawns
# powershell.exe by bare name, so we have to pin the standard System32
# dirs (Wbem, WindowsPowerShell\v1.0) ourselves.
ENV PATH="C:\go\bin;C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0;C:\Windows\System32\OpenSSH"

# GOBIN=C:\go\bin so mage lands on PATH regardless of which user the runner
# spawns the job as (avoids the per-profile C:\Users\<X>\go\bin trap).
RUN $env:GOBIN='C:\go\bin'; go install github.com/magefile/mage@latest

RUN Invoke-WebRequest -Uri "https://download.docker.com/win/static/stable/x86_64/docker-${env:DOCKER_CLI_VERSION}.zip" -OutFile docker.zip; `
    Expand-Archive docker.zip -DestinationPath C:\temp-docker; `
    Copy-Item C:\temp-docker\docker\docker.exe C:\go\bin\docker.exe; `
    Remove-Item -Recurse docker.zip, C:\temp-docker

RUN New-Item -ItemType Directory -Force -Path C:\ephemerd-ci\pkg\runner\embed | Out-Null; `
    Invoke-WebRequest -Uri "https://github.com/actions/runner/releases/download/v${env:RUNNER_VERSION}/actions-runner-win-x64-${env:RUNNER_VERSION}.zip" `
        -OutFile "C:\ephemerd-ci\pkg\runner\embed\actions-runner-win-x64-${env:RUNNER_VERSION}.zip"; `
    Invoke-WebRequest -Uri "https://github.com/actions/runner/releases/download/v${env:RUNNER_VERSION}/actions-runner-linux-x64-${env:RUNNER_VERSION}.tar.gz" `
        -OutFile "C:\ephemerd-ci\pkg\runner\embed\actions-runner-linux-x64-${env:RUNNER_VERSION}.tar.gz"

RUN New-Item -ItemType Directory -Force -Path C:\ephemerd-ci\bin | Out-Null; `
    Invoke-WebRequest -Uri "https://github.com/golangci/golangci-lint/releases/download/v${env:GOLANGCI_LINT_VERSION}/golangci-lint-${env:GOLANGCI_LINT_VERSION}-windows-amd64.zip" `
        -OutFile lint.zip; `
    Expand-Archive lint.zip -DestinationPath C:\temp-lint; `
    Copy-Item "C:\temp-lint\golangci-lint-${env:GOLANGCI_LINT_VERSION}-windows-amd64\golangci-lint.exe" C:\ephemerd-ci\bin\golangci-lint.exe; `
    Remove-Item -Recurse lint.zip, C:\temp-lint

RUN New-Item -ItemType Directory -Force -Path C:\ephemerd-ci\pkg\vm\embed | Out-Null; `
    New-Item -ItemType File -Path C:\ephemerd-ci\pkg\vm\embed\ephemerd-linux | Out-Null

COPY entrypoint-cache.ps1 C:\ephemerd-ci\entrypoint-cache.ps1
