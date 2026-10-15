#!/bin/sh
set -eu
cd "$(dirname "$0")"
mkdir -p dist
docker run --rm \
  -v "$PWD:/src" \
  -v cpa-retry-go-cache:/root/.cache/go-build \
  -v cpa-retry-go-mod:/go/pkg/mod \
  -w /src golang:1.26-bookworm \
  sh -c 'export PATH=/usr/local/go/bin:$PATH; go test -race ./... && go build -buildmode=c-shared -trimpath -o dist/anyrouter-retry-v0.1.13.so .'
