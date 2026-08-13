# Load test results: single-process capacity

Measurements of how far one signaling process goes on a 1 vCPU / 512 MB
instance, and where its memory actually goes. Run on 2026-08-12.

The short version: this server is **memory-bound, not CPU-bound**, and the
memory was dominated by fixed per-socket overhead rather than by anything
proportional to signaling traffic. Removing that overhead — the hijacked
`http.conn`, the WebSocket library's per-connection buffers, and both
per-connection goroutines — took a socket from 27.9 KB to **9.3 KB** and the
ceiling of a 512 MB instance from ~15 000 sockets to **55 000**.

The one CPU-side change measured here came later: relaying the signal payload
without decoding and re-encoding it made the relay path ~39% faster in
isolation, worth ~1.8 points of a core end to end. See [Relaying the payload
without re-encoding it](#relaying-the-payload-without-re-encoding-it).

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

Everything here is reproducible with the harness in
[`loadtest/`](../loadtest/README.md), which is what produced it:

```sh
cd loadtest && ./build.sh
./run.sh -mode hold -rooms 10000 -ramp 40s -hold 30s
```

Go 1.25.5; gobwas/ws v1.4.0 (gorilla/websocket v1.5.3 remains as the test
client). "Peak RSS" is the container's `memory.current`, so it includes kernel
socket memory charged to the cgroup, not just the Go heap. Per-socket figures
divide by concurrent sockets and therefore fold in the ~25 MB the process uses
at rest.

Peak RSS moves by roughly ±10% between identical runs, depending on where a GC
cycle lands relative to the once-a-second sampler — six repeats of the 10 000
socket run on the final build gave 108.5, 109.0, 109.1, 110.8, 121.7 and
133.7 MB. Figures below are representative runs, and the differences they are
used to argue are much larger than that.

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
| gobwas + epoll | 65 000 | **OOM-killed at ~58 500** (90.6%) | 512 MB (limit) |

55 000 sockets is the edge — 509.5 MB against a 512 MB limit, with no headroom
for a GC cycle that arrives at a bad moment. 45 000 is the last size with room
to spare. A run ramped at 65 000 puts the true wall at **~58 500 sockets**:
the container hits its limit and the process is OOM-killed (exit 137,
`OOMKilled: true`), after which every remaining dial is refused.

## The room ID ceiling

At 45 000 and above, runs used to intermittently produce one or two
`1302 could not allocate room id` errors -- two of the three runs at 45 000
did, one did not. That was the room ID allocator running out of its 5 retries
against a room table that large, not a memory or transport limit, and it was
the first non-memory ceiling this server hit as capacity grew.

Widening the nid's **last** digit from base 10 to Crockford base32, matching
its first digit, took the pool per shard from 32 × 10⁴ = 320 000 to
32 × 10³ × 32 = **1 024 000** (see `docs/ROOM_ID_SPEC.md`). Retry exhaustion
is quintic in occupancy — with `p` of the pool taken, a create fails only if
all 5 candidates collide, at rate `p⁵` — so 3.2x the pool is 3.2⁵ ≈ **340x**
fewer failures at the same room count:

| Live rooms | Occupancy (old / new) | Expected `1302` per full ramp (old / new) |
|---|---|---|
| 45 000 | 14.1% / 4.4% | 2.5 / 0.007 |
| 55 000 | 17.2% / 5.4% | 8.4 / 0.025 |

Measured, same 55 000-socket `hold` run on both builds:

| Build | `1302` errors | Peak RSS |
|---|---|---|
| 4-digit nid (old) | 1 | 496.1 MB |
| base32 last digit (new) | 0 | 478.6 MB |
| base32 last digit (new), repeat | 0 | 493.1 MB |

A clean 45 000 run on the new build also showed 0 (485.5 MB). Keeping retry
exhaustion under 1 in 10⁵ creates needs occupancy below ~10%, which is
102 400 live rooms on the new pool against 32 000 on the old. **Room ID
allocation has stopped being the binding ceiling**: it now sits at roughly
1.8x the memory wall instead of well below it, and memory is once again what
this server runs out of first.

The peak RSS figures above are all within the ±10% run-to-run noise of each
other and of the 509.5 MB in the table — the ID schema does not change the ID
length, so it does not move memory. Note that the 55 000 run at 478.6 MB and
the 45 000 run at 485.5 MB are in the wrong order relative to each other for
exactly that reason: where the GC cycle lands dominates at this scale.

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

That 25% is what the next section goes after.

## Relaying the payload without re-encoding it

Run on 2026-08-13, against the build measured above.

A signal's `data` field is opaque to this server — the design doc says so — but
it was not treated that way. `data` was decoded into a Go `string` on the way
in and re-encoded on the way out, and the payload is itself JSON
(`JSON.stringify(sdp)`), so nearly every byte of it is an escaped quote:
unquoting it and re-quoting it is work proportional to the largest field in the
message, done twice, to arrive back at the bytes that came in.

The relay now splices the raw `data` token straight from the inbound message
into the outbound one (`protocol.AppendSignalResponse`), and writes the small
fixed fields around it by hand rather than through `json.Marshal`'s reflection.
Nothing else about the message changes — the encoder is tested byte-identical
to marshaling the struct, HTML escaping and all.

Isolated, the relay path is **~39% faster and allocates ~24% less**
(`BenchmarkSignalRelay`, which decodes a signal off the wire, relays it, and
encodes the `signal-response`; 6×3000 iterations per size, `benchstat`,
p=0.002; dev laptop, not the container):

| SDP size | Before | After | Δ | Allocs |
|---|---|---|---|---|
| 1 KB | 9.63 µs | 5.74 µs | **-40.4%** | 12 → 10 |
| 2 KB | 15.76 µs | 10.04 µs | **-36.3%** | 12 → 10 |
| 8 KB | 51.40 µs | 31.09 µs | **-39.5%** | 12 → 10 |

Bytes allocated per relayed signal fall by 22–25% at every size: 9.46 KiB →
7.11 KiB at the 2 KB payload the load test uses. The two allocations removed
are the decoded payload string and `json.Marshal`'s output buffer.

In the container the saving is real but much smaller, because the relay is not
most of what the process does. `pair` mode at 600 rooms/s for 30 s, the two
images alternating within each round so host noise hits both; normalized is CPU
per 1000 signals relayed, which corrects for the generator not always
delivering the same offered load (round 6's `before` run is 4% short):

| Round | Before | After | Δ normalized |
|---|---|---|---|
| 1 | 78.1% | 75.2% | -3.5% |
| 2 | 78.0% | 75.0% | -3.7% |
| 3 | 73.9% | 72.6% | -1.1% |
| 4 | 74.0% | 72.3% | -1.9% |
| 5 | 73.8% | 72.5% | -2.1% |
| 6 | 72.1% | 72.1% | -3.8% |
| 7 | 74.3% | 73.7% | -0.8% |
| **mean** | **74.9%** | **73.3%** | **-2.4%** |

**~1.8 points of a core**, and the `after` run is cheaper in all 7 rounds.

A CPU profile of a 20 s window at this rate agrees on where it went:
`encoding/json.Marshal` drops from 8.8% of samples to 4.9%, and none of what
remains is on the relay path — it is the handshake responses, which still
marshal structs. The hand-written encoder that replaced it costs 1.3% of
samples. `json.Unmarshal` is untouched at ~20% and is now the largest single
JSON cost by a wide margin.

The gain is proportional to payload bytes, which is worth knowing before
expecting it everywhere. The same A/B at 300 rooms/s with 24 ICE candidates per
side — 48 signals per room instead of 18, but all of them ~130-byte candidates
rather than 2 KB offers — measured 68.0% before and 68.1% after, i.e. nothing.
Escaping a small payload costs little, and what remains is the per-message
overhead this change does not touch. The saving is on SDP, and it grows with
SDP size, which is the direction real traffic moves: a multi-track offer is
larger than the 2 KB the generator sends, not smaller.

Two honest caveats. The container figure is a small difference between noisy
runs: single runs of either build vary by ±3 points of a core, which is larger
than the effect, so it shows up only as a paired comparison — the sign is
consistent, the magnitude is ±1 point. And `signal_bytes_relayed_total` now
counts encoded bytes rather than decoded ones, so it reads about 5% higher for
the same traffic (116.3 MB → 122.8 MB across otherwise identical runs); that is
a change in the metric's definition, not in the bytes on the wire.

Memory is unaffected at rest — nothing about this is per-socket — but it does
cut garbage on the signaling path by ~24% per relayed signal, which is the
allocation rate that has to be collected within `GOMEMLIMIT` under load.

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

1. **Memory** — the process is OOM-killed at ~58 500 sockets on 512 MB, and
   there is no longer a large fixed per-socket cost to remove.
2. **Room ID allocation** — no longer binding after the nid's last digit went
   base32 (see [The room ID ceiling](#the-room-id-ceiling)); the pool is good
   past 100 000 live rooms, well above the memory wall.
3. **CPU under signaling load** — still `encoding/json`, but only the decode
   half of it now that the payload is relayed without re-encoding (see
   [Relaying the payload without re-encoding
   it](#relaying-the-payload-without-re-encoding-it)). `json.Unmarshal` is ~20%
   of samples, and every message is parsed twice: once into `Envelope` to learn
   its `type`, then again into the typed struct. Both passes run
   `checkValid` over the whole payload, so a signal's SDP blob is scanned end
   to end four times before it is relayed. Getting the `type` out without a
   full parse — so the typed decode is the only pass — is the next saving, and
   it is worth roughly what the re-encode was.
4. **Per-message write syscalls** — 29% of samples, one `write` per message.
   Only coalescing across messages would reduce it, which trades latency.
