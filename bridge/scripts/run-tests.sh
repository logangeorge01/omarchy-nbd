#!/bin/bash
set -u
export PATH="$PATH:/usr/local/go/bin"
cd "$(dirname "$0")/.."

echo "== go version =="
go version

echo "== go build ./... =="
CGO_ENABLED=0 go build ./...
echo "BUILD_EXIT=$?"

echo "== go vet ./... =="
CGO_ENABLED=0 go vet ./...
echo "VET_EXIT=$?"

echo "== go test ./... (no race, matches the static CGO_ENABLED=0 build) =="
CGO_ENABLED=0 go test -v ./... 2>&1
echo "TEST_EXIT=$?"

echo "== go test -race ./... (needs cgo; just for the concurrency check, not what ships) =="
CGO_ENABLED=1 go test -race -v ./... 2>&1
echo "RACE_TEST_EXIT=$?"
