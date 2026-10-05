#!/usr/bin/env bash
# Builds release archives and SHA256SUMS into dist/.
#   scripts/release.sh 0.1.0
# Reproducible: -trimpath, no build ids, fixed archive metadata, so anyone can rebuild a tag and
# compare hashes. Releases are cut from public GitHub Actions with SLSA provenance + Sigstore.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:?version, e.g. 0.1.0}
COMMIT=${GITHUB_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}
COMMIT=${COMMIT:0:12}
LDFLAGS="-s -w -buildid= -X main.version=$VERSION -X main.commit=$COMMIT"
rm -rf dist && mkdir -p dist

for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
  os=${target%/*}; arch=${target#*/}
  name="liftbay-runner_${VERSION}_${os}_${arch}"
  work=$(mktemp -d)
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$work/liftbay-runner" ./cmd/liftbay-runner
  touch -t 202001010000 "$work/liftbay-runner"
  # GNU tar flags when available for byte-identical archives; bsdtar otherwise.
  if tar --version 2>/dev/null | grep -q GNU; then
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1577836800 -C "$work" -cf - liftbay-runner | gzip -n > "dist/$name.tar.gz"
  else
    tar --uid 0 --gid 0 -C "$work" -cf - liftbay-runner | gzip -n > "dist/$name.tar.gz"
  fi
  rm -rf "$work"
  echo "built dist/$name.tar.gz"
done

(cd dist && shasum -a 256 *.tar.gz > SHA256SUMS)
cat dist/SHA256SUMS
