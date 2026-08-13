# Load test results: single-process capacity

Measurements of how far one signaling process goes on a 1 vCPU / 512 MB
instance, and where its memory actually goes. Run on 2026-08-12.

The short version: this server is **memory-bound, not CPU-bound**, and the
memory was dominated by fixed per-socket overhead rather than by anything
proportional to signaling traffic. Removing that overhead — the hijacked
`http.conn`, the WebSocket library's per-connection buffers, and both
per-connection goroutines — took a socket from 27.9 KB to **9.3 KB** and the
ceiling of a 512 MB instance from ~15 000 sockets to **55 000**.

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

Go 1.25.5; gobwas/ws v1.4.0 (gorilla/websocket v1.5.3 remains as the test
client). "Peak RSS" is the container's `memory.current`, so it includes kernel
socket memory charged to the cgroup, not just the Go heap. Per-socket figures
divide by concurrent sockets and therefore fold in the ~25 MB the process uses
at rest.

Load generator caveats, since they shape what the numbers can support:

- It runs on the same (much larger) host. Source addresses are spread over
  many `127.16.x.x` IPs and the container uses `--network=host`; without both,
  the generator exhausts the ephemeral port range and TIME\_WAIT sockets show
  up as server-side failures that are not the server's fault.
- `X-Forwarded-For` supplies distinct client IPs so the per-IP handshake limit
  (10/min, burst 20, hardcoded in `room.New`) does not bound the test.
- Held sockets need a reader from the moment they are created: gorilla answers
  server pings from inside `ReadMessage`, so a socket nobody is reading misses
  two pongs and is closed — correctly — for keepalive timeout. An earlier
  version of the generator only started its readers after the ramp, which made
  every run with a ramp longer than 90 s under-report live sockets.
- The sub-1% failures in the throughput runs are generator-side saturation,
  not server rejections: `signal_rate_limit_rejections_total` is 0 and the
  errors are client dial failures.

## Where the memory went

Four costs were paid by every socket for its whole life, none of them
proportional to how much signaling it did. In the order they were removed:

**1. The hijacked `http.conn` (7.9 KB/socket).** A heap profile at 10 000 idle
sockets attributed **42% of sampled heap to `bufio.NewReaderSize` and
`bufio.NewWriterSize`** — more than every signaling structure combined:

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
own goroutine lets the conn become garbage.

**2. gorilla's per-connection read buffer (3.5 KB/socket).** Replacing
gorilla/websocket with gobwas/ws, which is a framing library rather than a
connection type, means frames are parsed straight off the `net.Conn` with no
buffered reader parked on an idle socket.

**3. The writer goroutine and its send channel (4.9 KB/socket).** Every
connection had a second goroutine draining a 64-slot buffered channel — 1.6 KB
for the channel, the rest stack — to guarantee that `Send` never blocks under
the room mutex. A non-blocking `syscall.Write` gives the same guarantee for
nothing: the write either completes, which it does for a signaling message
against an empty socket buffer, or returns EAGAIN, and only then is a pending
buffer and a drain goroutine allocated.

**4. The read goroutine (8.2 KB/socket).** A goroutine parked in `Read` holds
an 8 KB stack that is never given back: Go does not shrink the stack of a
goroutine blocked in netpoll, and three forced GCs at 10 000 sockets moved
`StackInuse` not at all. Readability now comes from one epoll instance for the
whole process (`EPOLLONESHOT`), and a message is handled on a goroutine that
exits when the socket is drained. Stacks went from 82.4 MB to 0.5 MB and the
process from 20 006 goroutines to **8**.

## 10 000 concurrent idle sockets

`hold` mode: 10 000 half-open rooms, 40 s ramp, 30 s hold. All runs 100%
success.

| Build | Peak RSS | Per socket | Goroutines |
|---|---|---|---|
| gorilla, blocking handler | 366.4 MB | 36.6 KB | 20 006 |
| **gorilla, non-blocking handler** (shipped) | **279.1 MB** | **27.9 KB** | 20 006 |
| gobwas | 244.2 MB | 24.4 KB | 20 006 |
| gobwas + epoll, fallback path | 214.2 MB | 21.4 KB | 10 007 |
| **gobwas + epoll** | **110.6 MB** | **11.1 KB** | **8** |

The last two rows are the same code. The fourth is it forced onto its fallback
path — a goroutine per connection, no epoll — which is what a TLS connection or
a non-Linux build gets (see [Fallback](#fallback-path)). The gap between them
is what the poller is worth: 10.3 KB per socket, all of it goroutine.

Between the first and last row, one socket costs 3.3x less. Nothing about the
signaling protocol changed.

## Capacity ceiling

`hold` mode, ramped to the stated size and held 30 s:

| Build | Sockets | Result | Peak RSS |
|---|---|---|---|
| gorilla, blocking handler | 15 000 | **OOM-killed at ~13 700** (93.1%) | 512 MB (limit) |
| gorilla, non-blocking handler | 15 000 | 100% success | 412.6 MB |
| gobwas + epoll | 30 000 | 100% success | 297.3 MB |
| gobwas + epoll | 45 000 | 100% success | 407.6 MB |
| gobwas + epoll | 55 000 | 100% success | 509.5 MB |

55 000 sockets is the edge — 509.5 MB against a 512 MB limit, with no headroom
for a GC cycle that arrives at a bad moment. 45 000 is the last size with room
to spare.

At 45 000 and above, runs intermittently produce one or two
`1302 could not allocate room id` errors out of 45 000 creates -- two of the
three runs at that size did, one did not. That is the room ID allocator running
out of retries against a room table that large, not a memory or transport
limit, and it is the first non-memory ceiling this server will hit as capacity
grows.

## Throughput and CPU

`pair` mode at 600 rooms/s for 30 s, sockets closed at pairing so memory stays
bounded. All builds have the non-blocking handler.

| Build | Throughput | Signals relayed | CPU |
|---|---|---|---|
| gorilla | 597.6 rooms/s | 322 866 | 70.0% of a core |
| gobwas | 599.1 rooms/s | 323 550 | 69.9% of a core |
| gobwas + epoll | 596.9 rooms/s | 323 424 | 72.4% of a core |

The library swap is free. **The epoll transport costs ~2.5 points of a core**
at this rate, and a CPU profile says where: `epoll_ctl` (the one-shot re-arm)
and `epoll_wait` are ~4% of samples, against the goroutine-per-connection
model's zero. Spawning a goroutine per readable event does not show up.

For scale, `syscall.write` is 29% of CPU at this rate and `encoding/json` is
another 25%, so the transport is not where this server spends its time. It is
a good trade for a server whose binding constraint is memory: 2.5% CPU for 3x
the sockets.

## Fallback path

The poller needs a file descriptor, so a TLS connection terminated in-process
cannot use it, and neither can a non-Linux build. Both fall back to a goroutine
per connection sharing all the same parsing (`poller_other.go`, `readloop.go`).
That path measured 214.2 MB at 10 000 sockets — better than the shipped build,
worse than epoll by 2x.

It also has a trap worth recording: a blocking read loop holds its read buffer
for the life of the connection, so taking that buffer from the shared pool
pinned 16 KB per socket and cost 55 MB at 10 000 sockets (269.1 MB before the
fix). Buffer lifetime has to match the path: pooled and 16 KB for an event
goroutine that holds it for one message, owned and 4 KB for a loop that parks
in `Read` with it.

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

At ~9.3 KB/socket, 45 000 sockets measured good at 407.6 MB, and a conservative
operating budget of **40 000 sockets** (~370 MB, leaving headroom for GC and
burst traffic), one instance sustains roughly:

| `PEER_CONNECTED_GRACE_SEC` | Sockets held | Sustainable arrival rate | Bound by |
|---|---|---|---|
| 60 (default) | rate × 130 | ~300 rooms/s | memory (CPU ~37%) |
| 10 | rate × 30 | ~850 rooms/s | CPU |
| 1 | rate × 12 | ~850 rooms/s | CPU |

**The instance is now CPU-bound below a ~20 s grace period**, where before it
was memory-bound almost everywhere. The grace period is still the cheapest
capacity knob available, but it has stopped being the only one that matters:
above ~850 rooms/s, JSON and write syscalls are the wall.

The CPU-bound figure is extrapolated from the measured 597 rooms/s at 72% of a
core, not measured directly — above ~600 rooms/s the load generator was near
its own limit, so the true ceiling is bounded below by ~600 and estimated at
~850. Treat it as an order of magnitude, not a target.

### Sizing the caps

`MAX_CONNECTIONS_GLOBAL` defaults to 100 000, but 512 MB holds 55 000 sockets
measured. The protective cap therefore still sits ~2x above the real ceiling
and never fires — the process would be OOM-killed instead of shedding load,
taking every live room with it.

Sized to the instance, the server survives overload *and* delivers higher
throughput, because rejecting excess connections with HTTP 503 is far cheaper
than GC-thrashing against the memory limit. For a 512 MB instance:

```
MAX_CONNECTIONS_GLOBAL=40000
MAX_ROOMS_GLOBAL=20000
GOMEMLIMIT=400MiB
```

`GOMEMLIMIT` alone is not enough — at 3000 rooms/s an earlier run with
`GOMEMLIMIT=400MiB` and no connection cap GC-thrashed at 95.8% CPU and was
killed anyway. The connection cap is what keeps the live set small enough for
`GOMEMLIMIT` to be satisfiable.

## Remaining headroom

There is no longer a large fixed per-socket cost to remove. Of the ~9.3 KB a
socket now costs, most is kernel socket memory charged to the cgroup and Go
heap for room state; `newWSConn` itself is ~1 MB of sampled heap at 10 000
sockets, down from 12 MB.

The next constraints, in the order they will bite:

1. **Room ID allocation** — collisions and retry exhaustion appear at ~45 000
   live rooms (`1302`), well before memory does.
2. **CPU under signaling load** — `encoding/json` is ~25% of profile samples.
   Relaying the raw signal payload without re-encoding it would be the largest
   single saving.
3. **Per-message write syscalls** — 29% of samples, one `write` per message.
   Only coalescing across messages would reduce it, which trades latency.
