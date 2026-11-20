#!/bin/sh
# Build bsdk8s-mount-proxy for FreeBSD: build.sh [arch] -> ./bsdk8s-mount-proxy
set -eu
cd "$(dirname "$0")"
GOOS=freebsd GOARCH=${1:-arm64} CGO_ENABLED=0 go build -trimpath -o bsdk8s-mount-proxy .
