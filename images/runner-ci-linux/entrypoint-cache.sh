#!/bin/bash
set -euo pipefail

CACHE=/opt/ephemerd-ci
WORK="${GITHUB_WORKSPACE:-.}"

echo "Restoring cached build deps from ${CACHE}..."

for dir in pkg/runner/embed pkg/cni/embed pkg/containerd/embed; do
    mkdir -p "${WORK}/${dir}"
    cp -n "${CACHE}/${dir}/"* "${WORK}/${dir}/" 2>/dev/null || true
done

mkdir -p "${WORK}/bin"
cp -n "${CACHE}/bin/"* "${WORK}/bin/" 2>/dev/null || true

mkdir -p "${WORK}/pkg/vm/embed"
touch "${WORK}/pkg/vm/embed/ephemerd-linux"

echo "Build deps restored."
