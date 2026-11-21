#!/bin/sh
# Builds the release archives into OUTDIR; expects web/dist to be built (make web).
set -eu

[ $# -eq 2 ] || { echo "usage: release-build.sh VERSION OUTDIR" >&2; exit 2; }
version=$1 out=$2
[ -f web/dist/index.html ] || { echo "web/dist is empty: run make web first" >&2; exit 1; }
mkdir -p "$out"
ldflags="-s -w -X github.com/asterism-dev/observatory/internal/version.Version=$version"

build() { # name pkg os arch
	dir=$(mktemp -d)
	CGO_ENABLED=0 GOOS=$3 GOARCH=$4 go build -trimpath -ldflags "$ldflags" -o "$dir/$1" "$2"
	tar -czf "$out/$1_${version}_$3_$4.tar.gz" -C "$dir" "$1"
	rm -rf "$dir"
}

for os in linux darwin; do
	for arch in amd64 arm64; do
		build observatory ./cmd/observatory "$os" "$arch"
	done
done
# Nodes run Firecracker, which needs linux/amd64.
build observatory-node ./cmd/node linux amd64
build observatory-init ./cmd/init linux amd64

(cd "$out" && sha256sum ./*.tar.gz | sed 's| \./| |' > SHA256SUMS)
