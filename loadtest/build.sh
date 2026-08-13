#!/usr/bin/env bash
# Build the load-test image from ../server (or $SRC) and the load generator.
#
#   ./build.sh                  # current working tree
#   SRC=/path/to/other/server ./build.sh
#   IMAGE=signal-old ./build.sh
#
# The server is compiled from a copy of the tree with pprof/pprof.go dropped
# into cmd/server, so the container exposes profiles on :6060. Nothing under
# ../server is modified.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${SRC:-$HERE/../server}"
IMAGE="${IMAGE:-signal-loadtest}"
BUILD="$HERE/.build"

rm -rf "$BUILD/srv"
mkdir -p "$BUILD"
cp -r "$SRC" "$BUILD/srv"
cp "$HERE/pprof/pprof.go" "$BUILD/srv/cmd/server/zz_pprof.go"

( cd "$BUILD/srv" && CGO_ENABLED=0 go build -o "$BUILD/signal-server" ./cmd/server )
( cd "$HERE/loadgen" && go build -o "$BUILD/loadgen" . )

docker build -q -t "$IMAGE" -f "$HERE/Dockerfile" "$BUILD" >/dev/null
echo "built $IMAGE and $BUILD/loadgen"
