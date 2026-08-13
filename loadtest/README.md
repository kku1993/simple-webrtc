# Load test harness

Runs the signaling server in a resource-limited container and drives real
WebSocket clients at it, reporting peak memory, CPU and the server's own
counters for the run. This is what produced
[docs/LOAD\_TEST\_RESULTS.md](../docs/LOAD_TEST_RESULTS.md).

Requires Linux with cgroup v2, Docker, Go, and `curl`. The container runs with
`--network=host`, so the server and the generator share the host's network
namespace.

## Quick start

```sh
cd loadtest
./build.sh
./run.sh -mode hold -rooms 10000 -ramp 40s -hold 30s
```

```
=== server-side (1 CPU / 512m container) ===
peak RSS:          110.1 MB
peak live rooms:   10000
peak live sockets: 10000
avg CPU:           18.7% of 1 core
```

`build.sh` compiles `../server` and the generator into `.build/`, and builds
the `signal-loadtest` image. `run.sh` starts a fresh container, runs one
scenario, samples the container's cgroup counters once a second, and prints the
peaks. Arguments are passed through to the generator.

## The two modes

**`-mode hold`** ramps up N half-open rooms — each is one host socket that
created a room nobody joins — and holds them. This measures the memory ceiling,
which is the constraint that actually binds this server:

```sh
./run.sh -mode hold -rooms 45000 -ramp 120s -hold 30s
```

Ramp slowly enough that the generator is not the bottleneck: ~350 sockets/s is
comfortable, faster starts producing dial failures that look like server
errors but are not.

**`-mode pair`** drives full room lifecycles at an open-loop arrival rate:
create, join, offer/answer, trickle ICE both ways, `peer-connected`, and then
the server's socket release. This measures throughput, latency and CPU:

```sh
./run.sh -mode pair -rate 600 -dur 30s -waitrelease=false
```

`-waitrelease=false` drops each room at pairing instead of waiting out
`PEER_CONNECTED_GRACE_SEC`, which keeps memory bounded so the run measures CPU
rather than capacity. With it left on (the default), the reported `release`
percentiles show how long the server actually held sockets after both peers
reported connected — grace period plus up to one 5 s sweep tick.

## Generator flags

| Flag | Default | Meaning |
|---|---|---|
| `-mode` | `pair` | `pair` or `hold` |
| `-rate` | 50 | pair: room arrivals per second |
| `-dur` | 30s | pair: how long to keep arriving |
| `-waitrelease` | true | pair: wait for the server to release sockets |
| `-rooms` | 5000 | hold: how many half-open rooms |
| `-ramp` | 30s | hold: how long to take getting there |
| `-hold` | 20s | hold: how long to sit at that size |
| `-ice` | 8 | ICE candidates each side trickles |
| `-sdpkb` | 2 | approximate SDP offer/answer size |
| `-ips` | 0 | distinct `X-Forwarded-For` values (0 = one per connection) |
| `-srcips` | 4096 | source addresses to spread connections over |
| `-addr` | set by `run.sh` | signaling endpoint |

## Container knobs

Environment variables read by `run.sh`:

| Variable | Default | |
|---|---|---|
| `CPUS` | `1` | `--cpus`, and `GOMAXPROCS` unless set separately |
| `MEMORY` | `512m` | `--memory` and `--memory-swap` |
| `GRACE` | `1` | `PEER_CONNECTED_GRACE_SEC` |
| `MAXCONN` | `400000` | `MAX_CONNECTIONS_GLOBAL` |
| `GOMEMLIMIT` | unset | passed through if set |
| `IMAGE` | `signal-loadtest` | image to run |
| `PORT` | `8080` | server port |

The room and connection caps default far above any real ceiling on purpose: the
point of a capacity run is to find where the process actually dies, not to
confirm that a cap fires. See "Sizing the caps" in the results doc for what to
deploy.

## Profiling a run

The image carries a pprof listener on `:6060` (`pprof/pprof.go`, copied into
the server's `cmd/server` at build time — the shipped binary has no such
listener). While a run is in flight:

```sh
# where the heap went
go tool pprof -top -inuse_space loadtest/.build/signal-server \
  http://localhost:6060/debug/pprof/heap

# goroutine count, and total stack memory
curl -s 'localhost:6060/debug/pprof/goroutine?debug=1' | head -1
curl -s 'localhost:6060/debug/pprof/heap?debug=1' | grep -E '^# (Stack|HeapInuse) '

# 20s CPU profile under load
curl -s 'localhost:6060/debug/pprof/profile?seconds=20' -o cpu.prof
go tool pprof -top -nodecount=25 loadtest/.build/signal-server cpu.prof
```

## Comparing two builds

`build.sh` takes the server tree from `$SRC`, so an A/B against another commit
is a worktree away:

```sh
git worktree add /tmp/base <commit>
SRC=/tmp/base/server IMAGE=signal-base ./build.sh
IMAGE=signal-base ./run.sh -mode hold -rooms 10000 -ramp 40s -hold 30s
```

## Reading the results honestly

- **The generator shares the host.** It is a much larger machine than the
  container, but at high rates it saturates first. Above ~600 rooms/s the
  reported failures are the generator's dial failures, not server rejections —
  check `signal_rate_limit_rejections_total` and `signal_errors_total`, which
  stay at zero when the server is not the one failing.
- **Held sockets must be read from.** gorilla answers the server's pings from
  inside `ReadMessage`, so a socket nobody is reading misses two pongs and is
  closed for keepalive timeout — correctly. The generator starts a reader as
  soon as a socket is created; a version that waited until the end of the ramp
  quietly under-reported live sockets on any ramp longer than 90 s.
- **Source ports are the other silent ceiling.** Connections are spread over
  4096 `127.16.x.x` source addresses (`-srcips`), because a single source
  address runs out of ephemeral ports at ~28 000 sockets and the TIME\_WAIT
  backlog shows up as server-side failure.
- **Peak RSS is the cgroup's, not the heap's.** It includes kernel socket
  buffers charged to the container, which is the number that decides whether
  the process is OOM-killed.
- **CPU differences smaller than a few points need paired runs.** A single run
  varies by ±3 points of a core between repeats, so an A/B has to alternate the
  two images over several rounds and compare within each round — and normalize
  by `signal_signals_relayed_total`, since the generator does not always
  deliver the same offered load. For anything narrower than that, measure the
  code path directly: `go test ./internal/room/ -bench SignalRelay -count 6`
  in `../server` isolates one relayed signal, and `benchstat` will resolve a
  few percent where a container run cannot.
