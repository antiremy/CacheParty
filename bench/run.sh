#!/usr/bin/env bash
# Runs the benchmark scenario against CacheParty and lancachenet/monolithic and
# prints one markdown table. Extra arguments go to `bench load`, e.g.
#   ./run.sh -scenario chunks -clients 100
# ORIGIN_DELAY and ORIGIN_RATE (bytes/s per response) shape the fake CDN.
set -euo pipefail
cd "$(dirname "$0")"

dc() { docker compose -f compose.yml "$@"; }
trap 'dc down -v >/dev/null 2>&1' EXIT

first=1
for target in cacheparty monolithic; do
  dc down -v >/dev/null 2>&1
  dc up -d --build dns origin "$target" >&2
  out=$(dc run --rm load -target "$target:80" -label "$target" "$@")
  if [ "$first" = 1 ]; then echo "$out"; first=0; else echo "$out" | tail -n +3; fi
done
