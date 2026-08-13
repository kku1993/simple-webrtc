// Package metrics defines the Prometheus counters used by the signaling server.
//
// The exported surface matches the metrics listed in docs/DESIGN.md
// §"operational endpoints": rooms live, rooms created, connections live,
// signals relayed, bytes relayed, rejoins split by recreated true/false, errors
// by code, rate-limit rejections, plus a few additional operational counters
// (rooms expired, rooms released).
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus instruments.
type Metrics struct {
	RoomsLive        prometheus.Gauge
	ConnectionsLive  prometheus.Gauge
	RoomsCreated     prometheus.Counter
	RoomsExpired     prometheus.Counter
	RoomsReleased    prometheus.Counter
	SignalsRelayed   prometheus.Counter
	BytesRelayed     prometheus.Counter
	RejoinsRecreated prometheus.Counter
	RejoinsSame      prometheus.Counter
	ErrorsByCode     *prometheus.CounterVec
	RateLimitRejects prometheus.Counter
	SignalBufferOverflow prometheus.Counter

	registry *prometheus.Registry
	startedAt time.Time
}

// New constructs a Metrics registered with a fresh Prometheus registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry:  reg,
		startedAt: time.Now(),
		RoomsLive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "signal_rooms_live", Help: "Current number of live rooms.",
		}),
		ConnectionsLive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "signal_connections_live", Help: "Current number of live WebSocket connections.",
		}),
		RoomsCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_rooms_created_total", Help: "Total rooms created.",
		}),
		RoomsExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_rooms_expired_total", Help: "Rooms removed by expiry or peer deadline.",
		}),
		RoomsReleased: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_rooms_released_total", Help: "Rooms released after both peers connected.",
		}),
		SignalsRelayed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_signals_relayed_total", Help: "Signal messages relayed.",
		}),
		BytesRelayed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_bytes_relayed_total", Help: "Bytes of signal data relayed, as encoded on the wire (quotes and escapes included).",
		}),
		RejoinsRecreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_rejoins_recreated_total", Help: "Rejoins that recreated a lost room.",
		}),
		RejoinsSame: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_rejoins_same_total", Help: "Rejoins to an existing room.",
		}),
		ErrorsByCode: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signal_errors_total", Help: "Error responses by code.",
		}, []string{"code"}),
		RateLimitRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_rate_limit_rejections_total", Help: "Rate-limit rejections.",
		}),
		SignalBufferOverflow: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signal_buffer_overflow_total", Help: "Signal buffer overflow events.",
		}),
	}
	reg.MustRegister(
		m.RoomsLive, m.ConnectionsLive,
		m.RoomsCreated, m.RoomsExpired, m.RoomsReleased,
		m.SignalsRelayed, m.BytesRelayed,
		m.RejoinsRecreated, m.RejoinsSame,
		m.ErrorsByCode, m.RateLimitRejects, m.SignalBufferOverflow,
	)
	return m
}

// RecordError increments the per-code error counter.
func (m *Metrics) RecordError(code int) {
	m.ErrorsByCode.WithLabelValues(itoa(code)).Inc()
}

// Handler returns the HTTP handler for the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		Registry: m.registry,
	})
}

// SetLiveRooms sets the live-room gauge. Callers pass an already-loaded count;
// taking the address of the parameter and re-reading it atomically would only
// read this function's own copy, which guarantees nothing.
func (m *Metrics) SetLiveRooms(n int64) {
	m.RoomsLive.Set(float64(n))
}

// Uptime returns the server uptime.
func (m *Metrics) Uptime() time.Duration { return time.Since(m.startedAt) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
