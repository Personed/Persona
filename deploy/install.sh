#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# `install.sh go` deploys the opt-in Go image (deploy/go.Dockerfile); the Node
# service stays the default while the Go backend finishes the app RPCs.
case "${1:-node}" in
  node)
    compose=deploy/compose.yaml
    install="node apps/controller/src/install.ts /control /source"
    ;;
  go)
    compose=deploy/compose.go.yaml
    install="controller install /control /source"
    ;;
  *)
    echo "usage: sh deploy/install.sh [node|go]" >&2
    exit 2
    ;;
esac
docker info --format '{{.OSType}}' | grep -qx linux
docker compose -f "$compose" build
docker compose -f "$compose" run --rm --no-deps -v "$PWD:/source:ro" controller $install
docker compose -f "$compose" up -d
printf '%s\n' 'Persona: http://localhost:4310' "首次管理员码：docker compose -f $compose exec app cat /data/owner-code"
