#!/usr/bin/env bash
# Start the server in a resource-limited container, run one load generator
# scenario against it, and report peak container memory and CPU alongside the
# server's own gauges.
#
#   ./run.sh -mode hold -rooms 10000 -ramp 40s -hold 30s
#   ./run.sh -mode pair -rate 600 -dur 30s -waitrelease=false
#   GRACE=10 CPUS=2 MEMORY=1g ./run.sh -mode pair -rate 150 -dur 30s
#
# Every argument is passed through to the generator; see README.md, or
# `.build/loadgen -h`.
#
# Requires Linux with cgroup v2 (the memory and CPU figures are read out of
# /sys/fs/cgroup inside the container) and a container able to use the host
# network namespace.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD="$HERE/.build"

IMAGE="${IMAGE:-signal-loadtest}"
NAME="${NAME:-sigtest}"
CPUS="${CPUS:-1}"
MEMORY="${MEMORY:-512m}"
PORT="${PORT:-8080}"

# PEER_CONNECTED_GRACE_SEC. The default of 1 keeps sockets short-lived so
# throughput runs stay memory-bounded; raise it to model production.
GRACE="${GRACE:-1}"
# Room and connection caps are set far above any real ceiling on purpose, so
# the process fails by running out of memory rather than by shedding load.
MAXCONN="${MAXCONN:-400000}"

if [ ! -x "$BUILD/loadgen" ]; then
  echo "no build found; run ./build.sh first" >&2
  exit 1
fi

start_server() {
  docker rm -f "$NAME" >/dev/null 2>&1
  docker run -d --name "$NAME" \
    --cpus="$CPUS" --memory="$MEMORY" --memory-swap="$MEMORY" \
    --ulimit nofile=1048576:1048576 --network=host \
    -e SERVER_SECRET="$(head -c 48 /dev/urandom | base64 | tr -d '\n')" \
    -e ALLOWED_ORIGINS="https://load.test" -e SHARD_NAME="t" \
    -e TRUSTED_PROXY_COUNT=1 -e GOMAXPROCS="${GOMAXPROCS:-$CPUS}" \
    -e MAX_ROOMS_PER_IP=100000 -e MAX_ROOMS_GLOBAL=200000 \
    -e MAX_CONNECTIONS_GLOBAL="$MAXCONN" \
    -e PEER_CONNECTED_GRACE_SEC="$GRACE" \
    ${GOMEMLIMIT:+-e GOMEMLIMIT=$GOMEMLIMIT} \
    "$IMAGE" >/dev/null
  for _ in $(seq 30); do
    curl -sf "localhost:$PORT/healthz" >/dev/null 2>&1 && return 0
    sleep 0.3
  done
  echo "server failed to start:" >&2
  docker logs "$NAME" 2>&1 | tail -20 >&2
  exit 1
}

# Sample the container's cgroup counters and the server's own gauges once a
# second for the length of the run.
sample() {
  local out="$1"
  : > "$out"
  while true; do
    local mem cpu rooms conns m
    mem=$(docker exec "$NAME" cat /sys/fs/cgroup/memory.current 2>/dev/null)
    cpu=$(docker exec "$NAME" sh -c "grep usage_usec /sys/fs/cgroup/cpu.stat" 2>/dev/null | awk '{print $2}')
    m=$(curl -s --max-time 2 "localhost:$PORT/metrics" 2>/dev/null)
    rooms=$(echo "$m" | awk '/^signal_rooms_live /{print $2}')
    conns=$(echo "$m" | awk '/^signal_connections_live /{print $2}')
    echo "$(date +%s.%N) ${mem:-0} ${cpu:-0} ${rooms:-0} ${conns:-0}" >> "$out"
    sleep 1
  done
}

start_server
SAMPLE="$BUILD/sample.$$.txt"
sample "$SAMPLE" & SPID=$!
trap 'kill $SPID 2>/dev/null' EXIT

"$BUILD/loadgen" -addr "ws://127.0.0.1:$PORT/v1/signal" "$@"
RC=$?

sleep 1
kill $SPID 2>/dev/null; wait $SPID 2>/dev/null

echo ""
echo "=== server-side ($CPUS CPU / $MEMORY container) ==="
awk '{
  if ($2+0 > maxmem) maxmem = $2+0
  if ($4+0 > maxrooms) maxrooms = $4+0
  if ($5+0 > maxconns) maxconns = $5+0
  if (NR==1) { c0=$3+0; t0=$1+0 }
  if ($3+0 > 0) { c1=$3+0; t1=$1+0 }
}
END {
  printf "peak RSS:          %.1f MB\n", maxmem/1048576
  printf "peak live rooms:   %d\n", maxrooms
  printf "peak live sockets: %d\n", maxconns
  if (t1>t0) printf "avg CPU:           %.1f%% of 1 core\n", 100*((c1-c0)/1e6)/(t1-t0)
}' "$SAMPLE"

echo ""
curl -s "localhost:$PORT/metrics" | awk '
/^signal_rooms_created_total |^signal_rooms_released_total |^signal_signals_relayed_total |^signal_rate_limit_rejections_total |^signal_errors_total|^signal_bytes_relayed_total /{print "  "$0}'
rm -f "$SAMPLE"
exit $RC
