# Load test results: single-process capacity

Measurements of how far one signaling process goes on a 1 vCPU / 512 MB
instance, and where its memory actually goes. Run on 2026-08-12.

The short version: this server is **memory-bound, not CPU-bound**, and the
memory is dominated by fixed per-socket overhead rather than by anything
proportional to signaling traffic. At 10 000 concurrent sockets the process
sits at ~15% of one core. It dies of memory long before it runs out of CPU.

## Test setup

Server in Docker (`alpine:3.20`), hard-limited to one CPU and 512 MB:

```
--cpus=1 --memory=512m --memory-swap=512m --network=host
GOMAXPROCS=1  TRUSTED_PROXY_COUNT=1  PEER_CONNECTED_GRACE_SEC=1
MAX_ROOMS_PER_IP=100000  MAX_ROOMS_GLOBAL=200000  MAX_CONNECTIONS_GLOBAL=400000
```

The caps are set far above the real ceiling deliberately, so the process fails
by running out of memory rather than by shedding load — the point is to find
where the ceiling is, not to confirm that a cap works. See
[Sizing the caps](#sizing-the-caps) for what to actually deploy.

Go 1.25.5; gorilla/websocket v1.5.3; gobwas/ws v1.4.0. "Peak RSS" is the
container's `memory.current`, so it includes kernel socket memory charged to
the cgroup, not just the Go heap. Per-socket figures divide by concurrent
sockets and therefore fold in the ~25 MB the process uses at rest.

Load generator caveats, since they shape what the numbers can support:

- It runs on the same (much larger) host. Source addresses are spread over
  many `127.16.x.x` IPs and the container uses `--network=host`; without both,
  the generator exhausts the ephemeral port range and TIME\_WAIT sockets show
  up as server-side failures that are not the server's fault.
- `X-Forwarded-For` supplies distinct client IPs so the per-IP handshake limit
  (10/min, burst 20, hardcoded in `room.New`) does not bound the test.
- The sub-1% failures in the throughput runs are generator-side saturation,
  not server rejections: `signal_rate_limit_rejections_total` is 0 and the
  errors are client dial failures.

## Where the memory goes

At 10 000 idle sockets, a heap profile of the pre-fix server attributed **42%
of sampled heap to `bufio.NewReaderSize` and `bufio.NewWriterSize`** — more
than every signaling structure combined:

```
22.08MB 22.35%  bufio.NewWriterSize
19.57MB 19.81%  bufio.NewReaderSize
12.02MB 12.16%  server.newWSConn
 7.18MB  7.27%  lru.New[int]
```

Those are net/http's buffers for the hijacked connection. `handleSignal` ran
the read loop inline, so the handler goroutine never returned, net/http could
never discard the `http.conn`, and each socket pinned a 4 KB `bufio.Reader`
plus a 4 KB `bufio.Writer` for its entire life. Running the read loop on its
own goroutine (`go c.runReadLoop(s, sess)`) lets the conn become garbage. The
goroutine count is unchanged — the handler goroutine exits where it used to
block.

This was the single largest win, and it is independent of the WebSocket
library.

## 10 000 concurrent idle sockets

`hold` mode: 10 000 half-open rooms, 40s ramp, 30s hold. All runs 100%
success.

| Build | Peak RSS | Per socket | Heap in use | Stack in use |
|---|---|---|---|---|
| gorilla, blocking handler | 366.4 MB | 36.6 KB | 190.6 MB | 117.6 MB |
| gobwas, blocking handler | 348.7 MB | 34.9 KB | 172.4 MB | 116.8 MB |
| **gorilla, non-blocking handler** | **287.1 MB** | **28.7 KB** | 103.3 MB | 117.6 MB |
| gobwas, non-blocking handler | 247.6 MB | 24.8 KB | 85.5 MB | 99.2 MB |

Reading the rows against each other:

- **Releasing the `http.conn` saves 7.9 KB/socket** (366.4 → 287.1 MB), almost
  all of it heap. This is the change that shipped.
- **Swapping gorilla for gobwas saves 1.8 KB/socket on its own** (366.4 →
  348.7 MB) — modest, because pooling the write buffer and shrinking the read
  buffer to 1 KB had already recovered most of what the swap would give.
- **Its benefit grows to 4.0 KB/socket once the http buffers are gone** (287.1
  → 247.6 MB): 1.8 KB of heap plus ~1.9 KB of goroutine stack, gobwas's read
  path being shallower than gorilla's.

Both libraries hold 20 006 goroutines for 10 000 sockets — two per connection,
a read loop and a writer.

## Throughput and CPU

`pair` mode at 600 rooms/s for 30s, sockets closed at pairing so memory stays
bounded. Both builds have the non-blocking handler.

| Build | Completed | Throughput | Signals relayed | CPU |
|---|---|---|---|---|
| gorilla | 17 820 | 594.0 rooms/s | 320 760 | 69.4% of a core |
| gobwas | 17 822 | 594.0 rooms/s | 320 796 | 67.8% of a core |

Identical work, and the difference is inside run-to-run noise. Neither the
non-blocking handler nor gobwas costs measurable CPU. In particular, gobwas
reads frame headers straight off the `net.Conn` with no buffered reader — the
extra syscall per frame header does not show up at this rate.

## Capacity ceiling

`hold` mode at 15 000 sockets, 60s ramp:

| Build | Result | Peak RSS |
|---|---|---|
| gorilla, blocking handler | **OOM-killed at ~13 700 sockets** (93.1% success) | 512 MB (limit) |
| gorilla, non-blocking handler | 15 000, 100% success | 412.6 MB |
| gobwas, non-blocking handler | 15 000, 100% success | 378.6 MB |

The shipped fix is sufficient to clear 15 000 sockets on a 512 MB instance.
gobwas buys headroom beyond that, not the capability itself — which is why the
port is parked on `experiment/gobwas-ws` rather than merged.

## What this means for deployment

Concurrent sockets, not room arrival rate, is the constraint:

```
live sockets ≈ arrival_rate × 2 × (PEER_CONNECTED_GRACE_SEC + 5s)
```

The `+5s` is the sweep interval in `room/sweep.go`; release happens on a
ticker, so a socket's real lifetime is the grace period plus up to one tick.
This model held to within 1% across runs (30 rooms/s at grace 60 predicted
3900 sockets, measured 3896; 150 rooms/s at grace 10 predicted 4500, measured
4476).

At ~28.7 KB/socket, 15 000 sockets measured good at 412.6 MB, and a
conservative operating budget of **12 000 sockets** (~370 MB, leaving headroom
for GC and burst traffic), one instance sustains roughly:

| `PEER_CONNECTED_GRACE_SEC` | Sockets held | Sustainable arrival rate | Bound by |
|---|---|---|---|
| 60 (default) | rate × 130 | ~90 rooms/s | memory (CPU ~11%) |
| 10 | rate × 30 | ~400 rooms/s | memory (CPU ~46%) |
| 1 | rate × 12 | ~850 rooms/s | CPU |

**The grace period is a ~9× capacity knob**, and by far the cheapest one
available. Nothing else in the configuration comes close. It also decides
*which* resource binds: at the default the instance is memory-saturated and
nearly CPU-idle, and only below ~2s does CPU become the limit.

The CPU-bound figure is extrapolated from the measured 594 rooms/s at 69% of a
core, not measured directly — above ~600 rooms/s the load generator was near
its own limit, so the true ceiling is bounded below by ~600 and estimated at
~850. Treat it as an order of magnitude, not a target.

### Sizing the caps

`MAX_CONNECTIONS_GLOBAL` defaults to 100 000, but 512 MB holds 15 000 sockets
measured, ~17 000 extrapolated to the limit. The protective cap therefore sits
~6× above the real ceiling and never fires — the process is OOM-killed instead
of shedding load, taking every live room with it.

Sized to the instance, the server survives overload *and* delivers higher
throughput, because rejecting excess connections with HTTP 503 is far cheaper
than GC-thrashing against the memory limit. For a 512 MB instance:

```
MAX_CONNECTIONS_GLOBAL=12000
MAX_ROOMS_GLOBAL=6000
GOMEMLIMIT=400MiB
```

`GOMEMLIMIT` alone is not enough — at 3000 rooms/s an earlier run with
`GOMEMLIMIT=400MiB` and no connection cap GC-thrashed at 95.8% CPU and was
killed anyway. The connection cap is what keeps the live set small enough for
`GOMEMLIMIT` to be satisfiable.

## Remaining headroom

After both changes, the largest single item is **goroutine stacks: ~10–12 KB
per socket**, two goroutines per connection, which is 40%+ of the total.
Eliminating the per-connection writer goroutine would outweigh the entire
gorilla→gobwas swap. That is the next thing to try if per-socket memory
becomes binding again.
