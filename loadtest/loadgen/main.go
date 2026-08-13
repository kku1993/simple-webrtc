// Load generator for the simple-webrtc signaling server.
//
// Two modes:
//
//	-mode=pair  open-loop arrival of full room lifecycles at -rate rooms/sec.
//	            Measures end-to-end pairing latency and error rate. This is the
//	            capacity number that matters, because rooms are ephemeral.
//	-mode=hold  ramp -rooms half-open rooms (host created, no guest) and hold
//	            them, to find the memory ceiling for concurrent live rooms.
//
// Every virtual client presents a unique X-Forwarded-For so the server's
// per-IP rate limits behave as they would for real, distributed traffic.
// Run the server with TRUSTED_PROXY_COUNT=1 for this to take effect.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	addr        = flag.String("addr", "ws://127.0.0.1:8080/v1/signal", "signaling endpoint")
	origin      = flag.String("origin", "https://load.test", "Origin header")
	mode        = flag.String("mode", "pair", "pair | hold")
	rate        = flag.Int("rate", 50, "pair mode: room arrivals per second")
	dur         = flag.Duration("dur", 30*time.Second, "pair mode: test duration")
	rooms       = flag.Int("rooms", 5000, "hold mode: number of half-open rooms")
	rampDur     = flag.Duration("ramp", 30*time.Second, "hold mode: ramp duration")
	holdFor     = flag.Duration("hold", 20*time.Second, "hold mode: hold duration after ramp")
	iceN        = flag.Int("ice", 8, "ICE candidates each side trickles")
	sdpKB       = flag.Int("sdpkb", 2, "approx SDP offer/answer size in KB")
	releaseWait = flag.Duration("releasewait", 90*time.Second, "max wait for the server's post-connect socket release")
	numIPs      = flag.Int("ips", 0, "distinct X-Forwarded-For values (0 = unique per connection)")
	srcIPs      = flag.Int("srcips", 4096, "spread source addresses over this many 127.16.x.x IPs to avoid ephemeral port exhaustion")
	waitRelease = flag.Bool("waitrelease", true, "hold sockets until the server releases them (realistic); false measures pairing throughput only")
)

// ---- metrics ----

var (
	okCount    atomic.Int64
	failCount  atomic.Int64
	errMu      sync.Mutex
	errKinds   = map[string]int{}
	latMu      sync.Mutex
	latencies  []time.Duration
	relMu      sync.Mutex
	releaseLat []time.Duration
)

func recordErr(kind string) {
	failCount.Add(1)
	errMu.Lock()
	errKinds[kind]++
	errMu.Unlock()
}

func recordOK(d time.Duration) {
	okCount.Add(1)
	latMu.Lock()
	latencies = append(latencies, d)
	latMu.Unlock()
}

// ---- helpers ----

var ipCounter atomic.Uint64

// uniqueIP hands each virtual client its own address in 10.0.0.0/8 so per-IP
// rate limits don't collapse the whole test onto one bucket.
func uniqueIP() string {
	n := ipCounter.Add(1)
	if *numIPs > 0 {
		n = n % uint64(*numIPs)
	}
	return fmt.Sprintf("10.%d.%d.%d", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
}

func epoch() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type conn struct {
	ws *websocket.Conn
}

// srcIP spreads outbound connections across loopback's 127.0.0.0/8 so the
// (src ip, src port, dst ip, dst port) tuple space is not capped by the ~28k
// ephemeral ports of a single source address. Without this the load generator
// exhausts ports into TIME_WAIT long before the server saturates.
func srcIP() *net.TCPAddr {
	if *srcIPs <= 1 {
		return nil
	}
	n := ipCounter.Load() % uint64(*srcIPs)
	return &net.TCPAddr{IP: net.IPv4(127, 16, byte((n>>8)&0xff), byte(n&0xff)|1)}
}

func dial() (*conn, error) {
	h := http.Header{}
	h.Set("Origin", *origin)
	h.Set("X-Forwarded-For", uniqueIP())
	d := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		NetDial: func(network, a string) (net.Conn, error) {
			return (&net.Dialer{LocalAddr: srcIP(), Timeout: 20 * time.Second}).Dial(network, a)
		},
	}
	ws, resp, err := d.Dial(*addr, h)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial http %d: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	return &conn{ws: ws}, nil
}

func (c *conn) send(v any) error {
	c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.ws.WriteJSON(v)
}

// readUntil returns the first message whose type is want. It fails fast on
// error-response so rate limiting shows up as a real failure rather than a
// timeout.
func (c *conn) readUntil(want string) (map[string]any, error) {
	deadline := time.Now().Add(45 * time.Second)
	for {
		c.ws.SetReadDeadline(deadline)
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("read(%s): %w", want, err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		t, _ := m["type"].(string)
		if t == "error-response" {
			return nil, fmt.Errorf("server error %v %v", m["errorCode"], m["message"])
		}
		if t == want {
			return m, nil
		}
	}
}

func (c *conn) close() {
	if c.ws != nil {
		c.ws.Close()
	}
}

// waitClosed waits for the server to release the socket after both peers
// report peer-connected (close code 4200).
// The server holds sockets for peerConnectedGraceSec (default 60s) after both
// peers report connected, so this deadline must exceed that.
func (c *conn) waitClosed() error {
	deadline := time.Now().Add(*releaseWait)
	for {
		c.ws.SetReadDeadline(deadline)
		_, _, err := c.ws.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok && ce.Code == 4200 {
				return nil
			}
			if websocket.IsCloseError(err, 4200) {
				return nil
			}
			return err
		}
	}
}

var sdpBlob, iceBlob string

func initPayloads() {
	// Rough stand-ins for a real SDP offer and trickled ICE candidates: the
	// server never inspects the body, only its size.
	sdpBlob = `{"type":"offer","sdp":"` + strings.Repeat("a=candidate:0 1 UDP 2130706431 192.168.1.1 5000 typ host\\r\\n", (*sdpKB*1024)/60) + `"}`
	iceBlob = `{"candidate":{"candidate":"candidate:842163049 1 udp 1677729535 203.0.113.7 54321 typ srflx raddr 0.0.0.0 rport 0","sdpMLineIndex":0,"sdpMid":"0"}}`
}

// ---- one full room lifecycle ----

func runRoom() {
	start := time.Now()

	host, err := dial()
	if err != nil {
		recordErr("host-dial: " + err.Error())
		return
	}
	defer host.close()

	if err := host.send(map[string]any{"type": "create-room", "hostEpoch": epoch()}); err != nil {
		recordErr("create-send: " + err.Error())
		return
	}
	cr, err := host.readUntil("create-room-response")
	if err != nil {
		recordErr("create: " + err.Error())
		return
	}
	roomID, _ := cr["roomId"].(string)

	guest, err := dial()
	if err != nil {
		recordErr("guest-dial: " + err.Error())
		return
	}
	defer guest.close()

	if err := guest.send(map[string]any{"type": "join-room", "roomId": roomID, "guestEpoch": epoch()}); err != nil {
		recordErr("join-send: " + err.Error())
		return
	}
	if _, err := guest.readUntil("join-room-response"); err != nil {
		recordErr("join: " + err.Error())
		return
	}
	if _, err := host.readUntil("guest-joined"); err != nil {
		recordErr("guest-joined: " + err.Error())
		return
	}

	// Offer / answer.
	if err := host.send(map[string]any{"type": "signal", "seq": 1, "data": sdpBlob}); err != nil {
		recordErr("offer-send: " + err.Error())
		return
	}
	if _, err := guest.readUntil("signal-response"); err != nil {
		recordErr("offer-recv: " + err.Error())
		return
	}
	if err := guest.send(map[string]any{"type": "signal", "seq": 1, "data": sdpBlob}); err != nil {
		recordErr("answer-send: " + err.Error())
		return
	}
	if _, err := host.readUntil("signal-response"); err != nil {
		recordErr("answer-recv: " + err.Error())
		return
	}

	// Trickle ICE both ways, interleaved.
	var wg sync.WaitGroup
	errCh := make(chan string, 4)
	trickle := func(from, to *conn, tag string) {
		defer wg.Done()
		for i := 0; i < *iceN; i++ {
			if err := from.send(map[string]any{"type": "signal", "seq": 2 + i, "data": iceBlob}); err != nil {
				errCh <- tag + "-send: " + err.Error()
				return
			}
		}
		for i := 0; i < *iceN; i++ {
			if _, err := to.readUntil("signal-response"); err != nil {
				errCh <- tag + "-recv: " + err.Error()
				return
			}
		}
	}
	wg.Add(2)
	go trickle(host, guest, "ice-h2g")
	go trickle(guest, host, "ice-g2h")
	wg.Wait()
	select {
	case e := <-errCh:
		recordErr(e)
		return
	default:
	}

	// Both report connected; server should release both sockets.
	if err := host.send(map[string]any{"type": "peer-connected"}); err != nil {
		recordErr("pc-host: " + err.Error())
		return
	}
	if err := guest.send(map[string]any{"type": "peer-connected"}); err != nil {
		recordErr("pc-guest: " + err.Error())
		return
	}

	// Pairing is done here. Socket release happens later on the registry's 5s
	// sweep tick, so it is timed separately -- folding it in would measure
	// sweep granularity rather than server capacity.
	recordOK(time.Since(start))

	if !*waitRelease {
		return
	}
	relStart := time.Now()
	if err := host.waitClosed(); err != nil {
		recordErr("release-host: " + err.Error())
		return
	}
	if err := guest.waitClosed(); err != nil {
		recordErr("release-guest: " + err.Error())
		return
	}
	relMu.Lock()
	releaseLat = append(releaseLat, time.Since(relStart))
	relMu.Unlock()
}

// ---- modes ----

func modePair() {
	interval := time.Second / time.Duration(*rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	stop := time.After(*dur)
	var wg sync.WaitGroup
	var inflight atomic.Int64

	go func() {
		t := time.NewTicker(2 * time.Second)
		for range t.C {
			fmt.Fprintf(os.Stderr, "  [%s] ok=%d fail=%d inflight=%d\n",
				time.Now().Format("15:04:05"), okCount.Load(), failCount.Load(), inflight.Load())
		}
	}()

loop:
	for {
		select {
		case <-stop:
			break loop
		case <-ticker.C:
			wg.Add(1)
			inflight.Add(1)
			go func() {
				defer wg.Done()
				defer inflight.Add(-1)
				runRoom()
			}()
		}
	}
	wg.Wait()
}

func modeHold() {
	step := *rampDur / time.Duration(*rooms)
	held := make([]*conn, 0, *rooms)
	// Every held socket needs a reader from the moment it is created, not from
	// the end of the ramp: gorilla answers the server's pings from inside
	// ReadMessage, so a socket nobody is reading misses two pongs and gets
	// closed for keepalive timeout on any ramp longer than 3x the ping
	// interval.
	readHeld := func(c *conn) {
		// No read deadline: gorilla handles pings inside ReadMessage without
		// returning, so a deadline would fire between server messages, end the
		// reader, and stop the pongs -- which the server would then correctly
		// treat as a dead peer. The reader ends when the socket is closed at
		// the end of the run.
		_ = c.ws.SetReadDeadline(time.Time{})
		for {
			if _, _, err := c.ws.ReadMessage(); err != nil {
				return
			}
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 200)

	for i := 0; i < *rooms; i++ {
		time.Sleep(step)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := dial()
			if err != nil {
				recordErr("dial: " + err.Error())
				return
			}
			if err := c.send(map[string]any{"type": "create-room", "hostEpoch": epoch()}); err != nil {
				recordErr("create-send: " + err.Error())
				c.close()
				return
			}
			if _, err := c.readUntil("create-room-response"); err != nil {
				recordErr("create: " + err.Error())
				c.close()
				return
			}
			okCount.Add(1)
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
			go readHeld(c)
		}()
		if i%500 == 0 {
			fmt.Fprintf(os.Stderr, "  ramp %d/%d ok=%d fail=%d\n", i, *rooms, okCount.Load(), failCount.Load())
		}
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "  ramp complete: ok=%d fail=%d; holding %s\n", okCount.Load(), failCount.Load(), *holdFor)

	// The readers started during the ramp keep answering pings.
	time.Sleep(*holdFor)
	for _, c := range held {
		c.close()
	}
}

func main() {
	flag.Parse()
	initPayloads()
	log.SetFlags(0)

	fmt.Printf("mode=%s addr=%s\n", *mode, *addr)
	start := time.Now()
	switch *mode {
	case "pair":
		fmt.Printf("target=%d rooms/s dur=%s ice=%d sdp=%dKB\n", *rate, *dur, *iceN, *sdpKB)
		modePair()
	case "hold":
		fmt.Printf("rooms=%d ramp=%s hold=%s\n", *rooms, *rampDur, *holdFor)
		modeHold()
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
	elapsed := time.Since(start)

	ok, fail := okCount.Load(), failCount.Load()
	fmt.Printf("\n=== results ===\n")
	fmt.Printf("elapsed:   %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("ok:        %d\n", ok)
	fmt.Printf("failed:    %d\n", fail)
	if ok+fail > 0 {
		fmt.Printf("success:   %.2f%%\n", 100*float64(ok)/float64(ok+fail))
	}
	if *mode == "pair" {
		fmt.Printf("throughput:%.1f rooms/s completed\n", float64(ok)/elapsed.Seconds())
	}

	latMu.Lock()
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p := func(q float64) time.Duration { return latencies[int(float64(len(latencies)-1)*q)] }
		fmt.Printf("latency:   p50=%s p90=%s p99=%s max=%s\n",
			p(0.50).Round(time.Millisecond), p(0.90).Round(time.Millisecond),
			p(0.99).Round(time.Millisecond), latencies[len(latencies)-1].Round(time.Millisecond))
	}
	latMu.Unlock()

	relMu.Lock()
	if len(releaseLat) > 0 {
		sort.Slice(releaseLat, func(i, j int) bool { return releaseLat[i] < releaseLat[j] })
		p := func(q float64) time.Duration { return releaseLat[int(float64(len(releaseLat)-1)*q)] }
		fmt.Printf("release:   p50=%s p90=%s max=%s  (grace + 5s sweep tick)\n",
			p(0.50).Round(time.Millisecond), p(0.90).Round(time.Millisecond),
			releaseLat[len(releaseLat)-1].Round(time.Millisecond))
	}
	relMu.Unlock()

	errMu.Lock()
	if len(errKinds) > 0 {
		type kv struct {
			k string
			v int
		}
		var list []kv
		for k, v := range errKinds {
			list = append(list, kv{k, v})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
		fmt.Printf("\ntop errors:\n")
		for i, e := range list {
			if i >= 8 {
				break
			}
			k := e.k
			if len(k) > 110 {
				k = k[:110]
			}
			fmt.Printf("  %5d  %s\n", e.v, k)
		}
	}
	errMu.Unlock()
}
