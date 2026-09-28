#!/usr/bin/env bash
# Chạy server và launcher ở nền. Log và pid nằm trong data/.
#   ADDR=127.0.0.1:8090 HARNESS=hermes MODEL= ./start.sh
set -euo pipefail
cd "$(dirname "$0")"
ADDR="${ADDR:-127.0.0.1:8090}"
HARNESS="${HARNESS:-hermes}"
MODEL="${MODEL:-}"
mkdir -p data

running() { [ -f "data/$1.pid" ] && kill -0 "$(cat "data/$1.pid")" 2>/dev/null; }

if running server; then
  echo "server đang chạy (pid $(cat data/server.pid))"
else
  nohup ./server -addr "$ADDR" -profiles profile -data data/server/tasks.json >> data/server.log 2>&1 &
  echo $! > data/server.pid
  echo "server pid $! -> data/server.log"
fi

for _ in $(seq 1 50); do
  curl -s "http://$ADDR/profiles" > /dev/null 2>&1 && break
  sleep 0.2
done

if running launcher; then
  echo "launcher đang chạy (pid $(cat data/launcher.pid))"
else
  args=(run -server "ws://$ADDR/ws" -harness "$HARNESS")
  [ -n "$MODEL" ] && args+=(-model "$MODEL")
  nohup ./launcher "${args[@]}" >> data/launcher.log 2>&1 &
  echo $! > data/launcher.pid
  echo "launcher pid $! -> data/launcher.log"
fi

sleep 1
curl -s "http://$ADDR/agent"; echo
