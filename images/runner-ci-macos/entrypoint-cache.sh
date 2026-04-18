#!/bin/bash
set -euo pipefail

CACHE="/opt/ephemerd-ci"
WORK="${1:-.}"
ARCH="$(uname -m)"

echo "Restoring cached build deps for macOS (${ARCH})..."

mkdir -p "${WORK}/pkg/runner/embed" "${WORK}/bin" "${WORK}/pkg/vm/embed"
cp -n "${CACHE}/pkg/runner/embed/"* "${WORK}/pkg/runner/embed/" 2>/dev/null || true

if [ "${ARCH}" = "arm64" ]; then
    cp -n "${CACHE}/bin/golangci-lint-arm64" "${WORK}/bin/golangci-lint" 2>/dev/null || true
    cp -n "${CACHE}/bin/mage-darwin-arm64" "${WORK}/bin/mage" 2>/dev/null || true
else
    cp -n "${CACHE}/bin/golangci-lint-amd64" "${WORK}/bin/golangci-lint" 2>/dev/null || true
    cp -n "${CACHE}/bin/mage-darwin-amd64" "${WORK}/bin/mage" 2>/dev/null || true
fi

touch "${WORK}/pkg/vm/embed/ephemerd-linux"
echo "Build deps restored."
