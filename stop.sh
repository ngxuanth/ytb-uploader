#!/usr/bin/env bash
# Dừng launcher (và phiên hermes đang chạy) rồi dừng server.
set -uo pipefail
cd "$(dirname "$0")"
for name in launcher server; do
  f="data/$name.pid"
  [ -f "$f" ] || continue
  pid=$(cat "$f")
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid"
    for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid"
    echo "đã dừng $name (pid $pid)"
  fi
  rm -f "$f"
done
