#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="$ROOT_DIR/dist"

mkdir -p "$DIST_DIR"

build() {
    local goos="$1"
    local goarch="$2"
    local target_dir="$3"
    local output_name="$4"

    mkdir -p "$DIST_DIR/$target_dir"
    echo "Building $target_dir/$output_name"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
        go build -trimpath -o "$DIST_DIR/$target_dir/$output_name" .
}

build linux amd64 linux deploy
build darwin amd64 macos-amd64 deploy
build darwin arm64 macos-arm64 deploy
build windows amd64 windows deploy.exe
