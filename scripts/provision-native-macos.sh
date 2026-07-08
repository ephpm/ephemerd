#!/usr/bin/env bash
# Provision a macOS host to run ephemerd native-mode jobs.
#
# WHY THIS EXISTS
#   Native mode ([runner.macos] mode = "native") runs GitHub Actions jobs
#   directly on the host — there is no per-job VM and no container image.
#   So every build dependency a workflow assumes is "already installed"
#   must actually exist on this host. In the VM path those deps were baked
#   into the macOS VM base disk image; native mode has no such base image,
#   so the host must be provisioned here instead.
#
#   Jobs run under sandbox-exec with /opt/homebrew mounted READ-ONLY, and
#   they use the host's Homebrew directly (HOMEBREW_PREFIX=/opt/homebrew).
#   That means a job can *use* installed formulae but cannot `brew install`
#   or `brew upgrade` anything. Two failure modes follow:
#     1. Missing formula   → the tool check fails deep in a build step, e.g.
#          spc doctor: "missing system commands: cmake"
#     2. OUTDATED formula  → a workflow step that runs `brew install <it>`
#          triggers an upgrade, which the sandbox blocks with:
#          "The following directories are not writable by your user:
#           /opt/homebrew ...". (This is exactly how a stale `llvm` failed
#          a php-sdk build.)
#   So this script both installs missing formulae AND upgrades outdated
#   ones — the host must be present *and* current.
#
# CONTRACT
#   Keep FORMULAE in sync with what your workflows expect. When a native
#   job fails on a missing/outdated tool, add it here (or just re-run this)
#   rather than patching one host by hand — that keeps every runner host
#   reproducible. Consider running this (or `brew upgrade`) on a schedule.
#
# USAGE
#   ./scripts/provision-native-macos.sh           # install + upgrade deps
#   ./scripts/provision-native-macos.sh --check   # report only, no changes
#
# Idempotent: safe to re-run. Homebrew is required and must already be
# installed at /opt/homebrew (Apple silicon).

set -euo pipefail

CHECK_ONLY=0
[[ "${1:-}" == "--check" ]] && CHECK_ONLY=1

BREW="${HOMEBREW_PREFIX:-/opt/homebrew}/bin/brew"
if [[ ! -x "$BREW" ]]; then
  echo "error: Homebrew not found at $BREW — install it first" >&2
  exit 1
fi

# Formulae required by native jobs. Extend as workflows need more.
#   llvm       — clang/LLVM toolchain; php-sdk's macOS build runs
#                `brew install llvm` and prepends it to PATH.
#   llvm@17    — libclang for Rust bindgen (ephpm):
#                LIBCLANG_PATH=$(brew --prefix llvm@17)/lib
#   autoconf/automake/libtool/pkgconf/bison/re2c/cmake/ninja
#              — static-php-cli (`spc doctor`) build toolchain for php-sdk.
FORMULAE=(
  llvm
  llvm@17
  autoconf
  automake
  libtool
  pkgconf
  bison
  re2c
  cmake
  ninja
)

missing=()
outdated=()
for f in "${FORMULAE[@]}"; do
  if "$BREW" --prefix "$f" >/dev/null 2>&1 && [[ -d "$("$BREW" --prefix "$f")" ]]; then
    if "$BREW" outdated --formula "$f" 2>/dev/null | grep -q .; then
      echo "OUTDATED: $f (jobs cannot upgrade — will be upgraded here)"
      outdated+=("$f")
    else
      echo "ok:       $f -> $("$BREW" --prefix "$f")"
    fi
  else
    echo "MISSING:  $f"
    missing+=("$f")
  fi
done

if [[ ${#missing[@]} -eq 0 && ${#outdated[@]} -eq 0 ]]; then
  echo "all native-runner deps present and up-to-date"
  exit 0
fi

if [[ $CHECK_ONLY -eq 1 ]]; then
  [[ ${#missing[@]}  -gt 0 ]] && echo "missing ${#missing[@]}: ${missing[*]}" >&2
  [[ ${#outdated[@]} -gt 0 ]] && echo "outdated ${#outdated[@]}: ${outdated[*]}" >&2
  exit 1
fi

if [[ ${#missing[@]} -gt 0 ]]; then
  echo "installing: ${missing[*]}"
  "$BREW" install "${missing[@]}"
fi
if [[ ${#outdated[@]} -gt 0 ]]; then
  echo "upgrading: ${outdated[*]}"
  "$BREW" upgrade "${outdated[@]}"
fi
echo "provisioning complete"
