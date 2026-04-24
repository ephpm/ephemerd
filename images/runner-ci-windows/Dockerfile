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

SHELL ["powershell", "-Command", "$ErrorActionPreference = 'Stop';"]

RUN Invoke-WebRequest -Uri "https://go.dev/dl/go${env:GO_VERSION}.windows-amd64.zip" -OutFile go.zip; `
    Expand-Archive go.zip -DestinationPath C:\; `
    Remove-Item go.zip

ENV PATH="C:\go\bin;C:\Users\ContainerUser\go\bin;${PATH}"

RUN go install github.com/magefile/mage@latest

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
