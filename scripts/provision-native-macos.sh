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
#   Symptom when a dep is missing: the job fails deep in a build step, e.g.
#     "Unable to find libclang: ... set the LIBCLANG_PATH environment
#      variable" (bindgen could not find /opt/homebrew/opt/llvm@17/lib).
#
# CONTRACT
#   Keep this list in sync with what your workflows expect. When a native
#   job fails on a missing tool/library, add the formula here rather than
#   patching one host by hand — that keeps every runner host reproducible.
#
# USAGE
#   ./scripts/provision-native-macos.sh          # install/verify deps
#   ./scripts/provision-native-macos.sh --check   # report only, no install
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
#   llvm@17 — libclang for Rust bindgen (LIBCLANG_PATH=$(brew --prefix llvm@17)/lib)
FORMULAE=(
  llvm@17
)

missing=()
for f in "${FORMULAE[@]}"; do
  if "$BREW" --prefix "$f" >/dev/null 2>&1 && [[ -d "$("$BREW" --prefix "$f")" ]]; then
    echo "ok:      $f -> $("$BREW" --prefix "$f")"
  else
    echo "MISSING: $f"
    missing+=("$f")
  fi
done

if [[ ${#missing[@]} -eq 0 ]]; then
  echo "all native-runner deps present"
  exit 0
fi

if [[ $CHECK_ONLY -eq 1 ]]; then
  echo "missing ${#missing[@]} formula(e): ${missing[*]}" >&2
  exit 1
fi

echo "installing: ${missing[*]}"
"$BREW" install "${missing[@]}"
echo "provisioning complete"
