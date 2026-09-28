#!/usr/bin/env bash
# Build từ source: bmcp (Node), launcher và server (Go).
set -euo pipefail
cd "$(dirname "$0")"
(cd resource/bmcp && npm ci && npm run build)
CGO_ENABLED=0 go build -o launcher ./cmd/launcher
CGO_ENABLED=0 go build -o server ./cmd/server
echo "built: ./launcher ./server resource/bmcp/dist/index.js"
