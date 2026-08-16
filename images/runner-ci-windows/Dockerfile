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
# Track the current Go stable release, not this repo's go.mod floor. This
# version is both what `go` resolves to on PATH and what actions/setup-go finds
# in the tool cache, and setup-go only hits the cache on an exact match — so
# the version that maximizes hits across the fleet is the newest stable, which
# is what `go-version: stable`, a `1.26` range, and an up-to-date go.mod all
# resolve to.
ARG GO_VERSION=1.26.6
# Additional Go versions to seed into the tool cache, space separated, e.g.
# "1.25.13 1.24.11". Empty by default: each extra version costs ~75 MB of pull
# and ~225 MB on disk (a full GOROOT, ~15,000 files), and these images are
# pulled onto every node. Widen this only when a repo on the fleet actually
# pins an older toolchain — the cost is paid by every job on every node, the
# benefit only by that repo.
ARG GO_EXTRA_TOOLCACHE_VERSIONS=""
ARG DOCKER_CLI_VERSION=27.3.1

# $ProgressPreference = 'SilentlyContinue' is required for Invoke-WebRequest
# in non-interactive PowerShell. Without it the cmdlet tries to render its
# progress bar to a non-existent console and hangs (or runs ~50× slower) on
# Windows containers — the symptom is RUN steps that download large files
# stalling indefinitely. See https://github.com/PowerShell/PowerShell/issues/2138
SHELL ["powershell", "-Command", "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]

# Install Go into the runner tool cache rather than C:\go, and seed any extra
# versions alongside it.
#
# The layout is the one actions/toolkit's tool-cache package defines and
# actions/setup-go consumes: <RUNNER_TOOL_CACHE>\<tool>\<version>\<arch> for
# the tool itself, plus a SIBLING marker FILE <...>\<arch>.complete. tc.find()
# returns a hit only when both exist, and findAllVersions() ignores any
# directory whose name is not a full semver — so the version directory must be
# x.y.z, never "1.26". <arch> is Node's os.arch(), i.e. "x64" on Windows amd64.
# The cached directory is GOROOT itself (setup-go descends into the archive's
# top-level "go" folder before caching), which is why we move go\ rather than
# the extraction root.
#
# This is a relocation, not a second copy: nothing is installed at C:\go
# anymore, so seeding the tool cache with the default single version costs the
# image nothing. RUNNER_TOOL_CACHE is set by ephemerd for every Windows job
# (pkg/runtime.WindowsToolCache) and repeated here so the image also works
# under a plain `docker run` of the runner; keep the two paths in sync.
RUN $versions = @($env:GO_VERSION) + (($env:GO_EXTRA_TOOLCACHE_VERSIONS -split '\s+') | Where-Object { $_ }); `
    foreach ($v in $versions) { `
        Invoke-WebRequest -Uri "https://go.dev/dl/go$v.windows-amd64.zip" -OutFile go.zip; `
        Expand-Archive go.zip -DestinationPath C:\go-extract; `
        New-Item -ItemType Directory -Force -Path "C:\hostedtoolcache\go\$v" | Out-Null; `
        Move-Item C:\go-extract\go "C:\hostedtoolcache\go\$v\x64"; `
        New-Item -ItemType File -Path "C:\hostedtoolcache\go\$v\x64.complete" | Out-Null; `
        Remove-Item -Recurse -Force go.zip, C:\go-extract `
    }

ENV RUNNER_TOOL_CACHE="C:\hostedtoolcache"

# Set PATH explicitly. ${PATH} substitution in Windows Dockerfiles is
# unreliable across SHELL changes; the GitHub Actions runner spawns
# powershell.exe by bare name, so we have to pin the standard System32
# dirs (Wbem, WindowsPowerShell\v1.0) ourselves. ${GO_VERSION} is a build ARG
# and is substituted by the Dockerfile parser, which is a different (and
# reliable) mechanism from the ${PATH} expansion the note above warns about.
#
# C:\go\bin stays on PATH and stays the GOBIN target even though GOROOT moved:
# it is where mage, docker.exe and anything a workflow `go install`s land, and
# both .github/workflows/ci.yml and build-images.yml write to it by name. A job
# that runs actions/setup-go prepends its own toolchain, so the tool cache
# entry here only ever serves as the default.
ENV PATH="C:\hostedtoolcache\go\${GO_VERSION}\x64\bin;C:\go\bin;C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0;C:\Windows\System32\OpenSSH"

# GOBIN=C:\go\bin so mage lands on PATH regardless of which user the runner
# spawns the job as (avoids the per-profile C:\Users\<X>\go\bin trap).
RUN New-Item -ItemType Directory -Force -Path C:\go\bin | Out-Null; `
    $env:GOBIN='C:\go\bin'; go install github.com/magefile/mage@latest

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
