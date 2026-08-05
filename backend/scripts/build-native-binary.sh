#!/usr/bin/env bash
# Build a native cerbix binary with version/commit embedded via ldflags.
set -euo pipefail

PKG="git.example.com/monitoring/cerbix"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-amd64}"
OUT="${OUT:-dist/release/cerbix-${GOOS}}"

mkdir -p "$(dirname "$OUT")"

LDFLAGS="-s -w -X ${PKG}/internal/buildinfo.Version=${VERSION} -X ${PKG}/internal/buildinfo.Commit=${COMMIT}"

echo "building ${OUT} (version=${VERSION} commit=${COMMIT} ${GOOS}/${GOARCH})"
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
  go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" -o "$OUT" ./cmd/cerbix

echo "built ${OUT}"
