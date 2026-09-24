#!/usr/bin/env bash
# Regenerate the Plugin API v1 Go code next to its definition, api/plugin/v1/plugin.proto.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PATH:$(go env GOPATH)/bin"
# -Iapi so the generated code registers the file as plugin/v1/plugin.proto, as the committed code does.
protoc -Iapi \
  --go_out=api --go_opt=paths=source_relative \
  --go-grpc_out=api --go-grpc_opt=paths=source_relative \
  api/plugin/v1/plugin.proto
