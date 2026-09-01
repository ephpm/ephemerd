# escape=`
# CI runner image for Windows with the MSVC toolchain pre-baked.
# Must be built on a Windows host.
#
# Parser directive `# escape=`` ` must be the very first line — before any
# regular comments — for both the legacy and buildkit Windows parsers to
# honor it. Without it, backtick line continuations get parsed as unknown
# instructions and Windows paths (C:\...) eat the next character.
#
# WHY THIS IMAGE EXISTS
#
# The ephpm/ephpm and ephpm/php-sdk Windows jobs (release.yml, nightly.yml,
# windows-php-check.yml, php-sdk's build.yml) each install VS Build Tools
# per-job via the official bootstrapper: ~14 min per job pulled from
# Microsoft's CDN, and failing roughly half the time around the 16.5-minute
# mark. Every one of those workflows guards the install with a fast-path —
#
#   Test-Path C:\BuildTools\MSBuild\Current\Bin\MSBuild.exe
#
# — so an image that already provides that exact layout turns the install
# step into a no-op with ZERO workflow changes. The same applies to their
# Rust step (Test-Path $env:USERPROFILE\.cargo\bin\rustup.exe). This image
# bakes both. Rollout is therefore: build + push this image, point the
# fleet's [runner.images.<repo>].windows at it, and the workflows
# self-discover the tools.
#
# WHAT MUST NOT CHANGE without checking every consumer first:
#   - the install path C:\BuildTools (all four workflows hardcode it, for
#     MSBuild, vcvars64.bat and the VC\Tools\Llvm libclang probe; php-sdk
#     additionally junctions "C:\Program Files\Microsoft Visual
#     Studio\2022\Community" -> C:\BuildTools per-job for spc's
#     findVisualStudio() — that junction stays a workflow concern, it costs
#     nothing and reparse points in image layers are a risk we don't need)
#   - the component set below (superset is fine, subset breaks the no-op:
#     a job whose fast-path hits but whose component is missing fails LATER
#     and more confusingly than the install it skipped)
#   - rustup under C:\Users\ContainerAdministrator (see the Rust step)
#
# SIZE: VS Build Tools with the VCTools workload + recommended components +
# Windows SDK is roughly 7-9 GB on disk, on top of the ~2 GB runner-ci-windows
# base. That is deliberate: the cost is paid once per node at pull time
# (and the base layers are already on every Windows node), against 14+ min
# of CDN download per job and the ~50% flake rate of installing VS on a
# fresh Server Core container. Do not "slim" this image by dropping
# --includeRecommended — the Windows SDK arrives through it.

# Chain from the existing Windows CI image rather than servercore directly:
# the Go tool cache, mage, docker CLI, runner archives and entrypoint-cache.ps1
# all carry over, every Windows node already has these layers, and this image
# stays a strict superset — a job that runs on runner-ci-windows runs
# identically here. build-images.yml orders this build after the base push
# (needs: windows) and builds with --pull so it never chains from a stale
# local tag.
FROM ephpm/ephemerd:runner-ci-windows

# The VS bootstrapper is fetched from the evergreen channel URL — the same
# URL the per-job installs use today, so the image and a cold-cache job
# install the same thing. VS Build Tools 17.x (VS 2022); the minor version
# floats with the channel, which is exactly what the workflows get now.
ARG VS_BUILDTOOLS_URL=https://aka.ms/vs/17/release/vs_buildtools.exe

# Pinned Rust toolchain. The workflows install `--default-toolchain stable`
# per-job; baking a floating "stable" would make image builds unreproducible,
# so pin the stable release current at bake time and bump it when the image
# is rebuilt. Must stay >= the MSRV of what the fleet builds (ephpm's floor
# is 1.88, forced by litewire). The default host triple of a Windows x64
# rustup install is x86_64-pc-windows-msvc, so the target the release
# workflows link with is included implicitly — no `rustup target add` needed.
ARG RUST_VERSION=1.98.0

# SHELL is redeclared even though the base image config carries one:
# inheriting a SHELL across a FROM is metadata behavior we'd rather not
# depend on silently. $ProgressPreference = 'SilentlyContinue' is required
# for Invoke-WebRequest in non-interactive PowerShell — without it the
# cmdlet tries to render a progress bar to a non-existent console and hangs
# (or runs ~50x slower) on Windows containers.
# See https://github.com/PowerShell/PowerShell/issues/2138
SHELL ["powershell", "-Command", "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';"]

# Disable Delivery Optimization BEFORE any heavy step, in its own layer.
#
# DoSvc runs inside Server Core containers and writes sparse temp files
# (C:\Windows\Temp\DO*.tmp) on its own schedule — whichever RUN happens to
# be live when it does gets the file in its layer, and the Windows layer
# archiver cannot serialize sparse backup streams, so the snapshot COMMIT
# of that step dies with:
#
#   failed to reimport snapshot: Files/Windows/Temp/DO****.tmp:
#   unknown stream ID 9        (9 = BACKUP_SPARSE_BLOCK)
#
# Observed killing this image's verification step (a read-only RUN!) on
# 2026-09-01 — the failure is nondeterministic and can land on ANY step,
# so it must be prevented at the source, not cleaned up after. Disabling
# the service is registry state, so this single layer protects every
# later step. Belt-and-braces: sweep any DO*.tmp it wrote before the
# disable landed. -ErrorAction on the service ops because a future base
# image may not ship DoSvc at all — absence is success here.
RUN Stop-Service DoSvc -Force -ErrorAction SilentlyContinue; `
    Set-Service DoSvc -StartupType Disabled -ErrorAction SilentlyContinue; `
    Remove-Item -Path 'C:\Windows\Temp\DO*.tmp' -Force -ErrorAction SilentlyContinue; `
    Write-Host 'Delivery Optimization disabled (sparse DO*.tmp files break Windows layer commits)'

# Install VS Build Tools into C:\BuildTools.
#
# The argument list is byte-for-byte the one the workflows use, and must
# stay a superset of every per-job install this image replaces:
#   - Workload.VCTools + --includeRecommended: MSBuild, cl.exe (VC.Tools),
#     the Windows 11 SDK, CMake — what php-sdk's MSVC lane installs.
#   - Component.VC.Llvm.Clang: clang-cl.exe and libclang.dll, which ephpm's
#     workflows add on top (bindgen needs libclang; the TAILCALL lane uses
#     its own pinned LLVM for PHP itself but still links with MSVC).
#
# Container-install caveats, each of which has bitten the per-job installs:
#   - Exit code 3010 is "success, reboot required" and MUST be treated as
#     success — containers cannot reboot, and Microsoft's own container
#     install docs special-case it. 0 and 3010 pass; anything else fails
#     the build after dumping the installer logs.
#   - On Server Core the bootstrapper has been observed to exit before the
#     underlying installer engine finishes (php-sdk's build.yml documents
#     this), so the exit code alone is not trusted in the success direction
#     either: we poll for MSBuild.exe, then hard-verify every binary the
#     workflows' fast-paths assume exists. A partial install that slipped
#     through here would turn the per-job install step into a no-op AND be
#     broken — the one failure mode this image must never ship.
#   - --nocache keeps the installer from persisting its package cache into
#     the layer; --norestart suppresses the reboot a container can't do;
#     --quiet --wait for the no-console environment.
#   - Building this image under Hyper-V isolation needs more than the
#     default container memory (Microsoft documents 2 GB as the floor for
#     the installer); see the note in build-images.yml.
#   - Installer logs (%TEMP%\dd_*.log) are deleted on success so they don't
#     bloat the layer; on failure their tails are printed, because a failed
#     `docker build` gives you no container to exec into.
RUN Write-Host 'Downloading VS Build Tools bootstrapper...'; `
    Invoke-WebRequest -Uri $env:VS_BUILDTOOLS_URL -OutFile C:\vs_buildtools.exe; `
    Write-Host 'Installing (this is the slow step - several GB from the CDN)...'; `
    $proc = Start-Process -FilePath C:\vs_buildtools.exe -ArgumentList @( `
        '--quiet', '--wait', '--norestart', '--nocache', `
        '--installPath', 'C:\BuildTools', `
        '--add', 'Microsoft.VisualStudio.Workload.VCTools', `
        '--add', 'Microsoft.VisualStudio.Component.VC.Llvm.Clang', `
        '--includeRecommended' `
    ) -Wait -PassThru -NoNewWindow; `
    Write-Host ('Bootstrapper exit code: ' + $proc.ExitCode); `
    if (($proc.ExitCode -ne 0) -and ($proc.ExitCode -ne 3010)) { `
        Write-Host '=== VS installer failed; recent dd_*.log tails ==='; `
        Get-ChildItem -Path $env:TEMP -Filter 'dd_*.log' -ErrorAction SilentlyContinue | `
            Sort-Object LastWriteTime -Descending | Select-Object -First 5 | `
            ForEach-Object { Write-Host ('--- ' + $_.Name + ' ---'); Get-Content $_.FullName -Tail 60 }; `
        exit 1 `
    }; `
    $msbuild = 'C:\BuildTools\MSBuild\Current\Bin\MSBuild.exe'; `
    $sw = [System.Diagnostics.Stopwatch]::StartNew(); `
    while (-not (Test-Path $msbuild) -and $sw.Elapsed.TotalMinutes -lt 30) { `
        Start-Sleep -Seconds 30; `
        Write-Host ('  ...waiting for VS install ({0:F0}s)' -f $sw.Elapsed.TotalSeconds) `
    }; `
    if (-not (Test-Path $msbuild)) { Write-Error 'MSBuild.exe never appeared - install engine died after the bootstrapper returned'; exit 1 }; `
    Remove-Item C:\vs_buildtools.exe; `
    Remove-Item -Path (Join-Path $env:TEMP 'dd_*.log') -Force -ErrorAction SilentlyContinue

# Verify, in a separate layer so a verification failure is unambiguous in
# the build log, every path a consumer workflow assumes exists. Each probe
# below is load-bearing for a specific fast-path or step:
#   - MSBuild.exe: THE fast-path sentinel in all four workflows.
#   - vcvars64.bat: "Enter MSVC developer environment" steps source it.
#   - cl.exe: the compiler vcvars64 puts on PATH (glob — the 14.x toolset
#     directory name floats with the channel).
#   - clang-cl.exe + libclang.dll: ephpm's bindgen (LIBCLANG_PATH probe) and
#     the clang-cl component generally.
#   - Windows SDK headers (windows.h): arrives via --includeRecommended into
#     C:\Program Files (x86)\Windows Kits; without it cl.exe compiles
#     nothing real and the failure surfaces deep inside a PHP build.
RUN $checks = [ordered]@{ `
        'MSBuild.exe'  = 'C:\BuildTools\MSBuild\Current\Bin\MSBuild.exe'; `
        'vcvars64.bat' = 'C:\BuildTools\VC\Auxiliary\Build\vcvars64.bat'; `
        'cl.exe'       = 'C:\BuildTools\VC\Tools\MSVC\*\bin\Hostx64\x64\cl.exe'; `
        'clang-cl.exe' = 'C:\BuildTools\VC\Tools\Llvm\x64\bin\clang-cl.exe'; `
        'libclang.dll' = 'C:\BuildTools\VC\Tools\Llvm\x64\bin\libclang.dll'; `
        'windows.h'    = 'C:\Program Files (x86)\Windows Kits\10\Include\*\um\windows.h' `
    }; `
    $failed = $false; `
    foreach ($c in $checks.GetEnumerator()) { `
        if (Test-Path $c.Value) { Write-Host ('OK   ' + $c.Key) } `
        else { Write-Host ('MISS ' + $c.Key + ' (' + $c.Value + ')'); $failed = $true } `
    }; `
    if ($failed) { Write-Error 'VS Build Tools layout verification failed - image would ship a broken fast-path'; exit 1 }

# Pre-install rustup + the pinned stable toolchain.
#
# Location matters more than it looks: the workflows' fast-path is
#   Test-Path "$env:USERPROFILE\.cargo\bin\rustup.exe"
# Docker build RUN steps on servercore execute as ContainerAdministrator,
# and job containers run as the image's default user — also
# ContainerAdministrator (neither this Dockerfile, its base, nor ephemerd
# sets USER; ephemerd carries the image config through via
# oci.WithImageConfig). So both sides of that Test-Path resolve to
# C:\Users\ContainerAdministrator\.cargo\bin\rustup.exe and the per-job
# rustup download becomes a no-op. Keep CARGO_HOME/RUSTUP_HOME at their
# defaults for the same reason — relocating them would dodge the fast-path.
#
# PATH is intentionally NOT touched: the workflows append $cargoBin to
# GITHUB_PATH themselves right after the fast-path check, and the MSVC env
# comes from sourcing vcvars64.bat per job — the image restating the base's
# PATH to append entries would have to re-derive the Go tool-cache path from
# a base ARG this file doesn't own. (The base's entrypoint-cache.ps1 note
# about Hyper-V PATH propagation applies here too.)
#
# --profile minimal matches the workflows (rustc, cargo, rust-std — no docs,
# no clippy: CI lint runs on Linux). rustup-init's URL is the same
# https://win.rustup.rs/x86_64 the workflows fetch.
#
# No double quotes anywhere in this step (Join-Path + single quotes only):
# a shell-form RUN's string reaches PowerShell through Windows
# command-line argument parsing, which eats embedded double quotes — on
# the first build the `"$env:USERPROFILE\.cargo\bin"` here arrived at the
# parser as `$env:USERPROFILE\.cargo\bin` (unquoted) and died with
# "Unexpected token '\.cargo\bin'". Every other RUN in this file is
# single-quote-only for the same reason; keep it that way.
RUN Write-Host ('Installing rustup with Rust ' + $env:RUST_VERSION + '...'); `
    Invoke-WebRequest -Uri 'https://win.rustup.rs/x86_64' -OutFile C:\rustup-init.exe; `
    & C:\rustup-init.exe -y --default-toolchain $env:RUST_VERSION --profile minimal; `
    if ($LASTEXITCODE -ne 0) { Write-Error ('rustup-init failed with exit code ' + $LASTEXITCODE); exit 1 }; `
    Remove-Item C:\rustup-init.exe; `
    $cargoBin = Join-Path $env:USERPROFILE '.cargo\bin'; `
    & (Join-Path $cargoBin 'rustup.exe') show; `
    & (Join-Path $cargoBin 'rustc.exe') --version; `
    & (Join-Path $cargoBin 'cargo.exe') --version; `
    if ($LASTEXITCODE -ne 0) { Write-Error 'baked Rust toolchain failed to run'; exit 1 }
