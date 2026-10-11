#!/bin/sh
# Builds the release binaries of one version, reproducibly: the same commit
# gives the same bytes on GitHub's runner and on any other machine. That is
# what lets a release be signed over binaries we built ourselves and still
# ship GitHub's (see RELEASING.md): the signer compares the two, byte for byte.
#
#   scripts/release/build.sh vX.Y.Z OUTDIR [goos/goarch ...]
#
# What makes it reproducible: the exact Go toolchain go.mod names (GOTOOLCHAIN,
# fetched and checksum-verified by the go command when it is not installed),
# CGO off, -trimpath (no build directory in the binary), no VCS stamp and an
# empty build id. Run it from a clean checkout of the release's commit.
set -eu

if [ $# -lt 2 ]; then
    echo "usage: $0 vX.Y.Z OUTDIR [goos/goarch ...]" >&2
    exit 2
fi
VERSION=$1
OUT=$2
shift 2
case "$VERSION" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) echo "error: $VERSION is not a release version (vX.Y.Z)" >&2; exit 2 ;;
esac
PLATFORMS=${*:-"linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64"}

cd "$(dirname "$0")/../.."
GOVERSION=$(awk '$1 == "go" { print $2; exit }' go.mod)
[ -n "$GOVERSION" ] || { echo "error: go.mod names no Go version" >&2; exit 1; }
export GOTOOLCHAIN="go$GOVERSION"
export CGO_ENABLED=0
export GOFLAGS="-trimpath -buildvcs=false"
mkdir -p "$OUT"

for p in $PLATFORMS; do
    goos=${p%/*}
    goarch=${p#*/}
    name="gpu-agent-$goos-$goarch"
    [ "$goos" = windows ] && name="$name.exe"
    GOOS=$goos GOARCH=$goarch go build -ldflags "-s -w -buildid= -X main.version=$VERSION" -o "$OUT/$name" ./cmd/gpu-agent/
    echo "built $OUT/$name"
done
