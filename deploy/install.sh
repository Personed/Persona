#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
docker info --format '{{.OSType}}' | grep -qx linux
docker compose -f deploy/compose.yaml build
docker compose -f deploy/compose.yaml run --rm --no-deps -v "$PWD:/source:ro" --entrypoint persona controller controller install /control /source
docker compose -f deploy/compose.yaml up -d
printf '%s\n' 'Persona: http://localhost:4310' '首次管理员码：docker compose -f deploy/compose.yaml exec app cat /data/owner-code'
