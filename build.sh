#!/bin/sh
# Build the restic sysext for ZimaOS.
#   sh build.sh [amd64|arm64]      (default: amd64)
# Output in dist/: restic-<arch>.raw (+ .sha256), for amd64 also restic.raw.
set -eu
cd "$(dirname "$0")"
ARCH="${1:-amd64}"
MODKIT="${LINTUX_MODKIT:-../lintux-modkit}"
[ -f "$MODKIT/sysext/add-web.sh" ] || { echo "lintux-modkit with sysext/add-web.sh not found at $MODKIT (set LINTUX_MODKIT)" >&2; exit 1; }
VERSION=$(cat VERSION)

# Upstream release, pinned by version AND sha256 (from SHA256SUMS of the release, 2026-10-23).
RESTIC_VERSION=0.19.1
case "$ARCH" in
  amd64) SUM=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c ;;
  arm64) SUM=a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 2 ;;
esac
BZ="restic_${RESTIC_VERSION}_linux_$ARCH.bz2"

rm -rf build
mkdir -p build/root build/dl
curl -fsSL -o "build/dl/$BZ" "https://github.com/restic/restic/releases/download/v$RESTIC_VERSION/$BZ"
echo "$SUM  build/dl/$BZ" | sha256sum -c -
bzip2 -dc "build/dl/$BZ" > build/dl/restic
chmod 755 build/dl/restic
# The backend tests run the real restic and restic-zima against a temp repository (amd64 build host).
go vet ./...
if [ "$ARCH" = amd64 ]; then RESTIC_BIN="$PWD/build/dl/restic" go test -count=1 ./...; else go test ./...; fi

cp -a files/. build/root/
chmod 755 build/root/usr/bin/restic-zima
install -m 755 build/dl/restic build/root/usr/bin/
mkdir -p build/root/usr/share/doc/restic
curl -fsSL -o build/root/usr/share/doc/restic/LICENSE "https://raw.githubusercontent.com/restic/restic/v$RESTIC_VERSION/LICENSE"
echo "restic $RESTIC_VERSION ($BZ sha256 $SUM)" > build/root/usr/share/doc/restic/packages.txt
cp README.md build/root/usr/share/doc/restic/README
# The tile: module JSON (its "services" make zpkg enable and start restic-web; without the
# JSON zpkg answers "module not installed", rc=1 — measured 1.8.0), UI files and the backend.
sh "$MODKIT/sysext/add-web.sh" restic build/root "$VERSION"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags="-s -w -X main.version=$VERSION" \
  -o build/root/usr/bin/restic-web ./cmd/restic-web
sh "$MODKIT/sysext/build-sysext.sh" restic build/root "$ARCH" dist "$VERSION"
cat "dist/restic-$ARCH.raw.sha256"
