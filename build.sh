#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p "$HOME/.local/bin"
gofmt -w main.go
go mod tidy
go build -tags sqlite_fts5 -o "$HOME/.local/bin/commsync" .

echo "installed: $HOME/.local/bin/commsync"
