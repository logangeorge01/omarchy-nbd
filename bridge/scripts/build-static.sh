#!/bin/bash
set -eu
export PATH="$PATH:/usr/local/go/bin"
cd "$(dirname "$0")/.."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o omarchy-nbd-bridge ./cmd/omarchy-nbd-bridge
./scripts/check-static.sh ./omarchy-nbd-bridge
