package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRegistersAll(t *testing.T) {
	m := New()
	if m.RoomsLive == nil || m.ConnectionsLive == nil || m.RoomsCreated == nil {
		t.Errorf("instruments not initialized")
	}
}

func TestRecordError(t *testing.T) {
	m := New()
	m.RecordError(1101)
	m.RecordError(1101)
	m.RecordError(1301)
	// Should not panic; counters are tracked by label.
}

func TestHandlerExposesMetrics(t *testing.T) {
	m := New()
	m.RoomsCreated.Inc()
	m.RoomsLive.Set(3)
	m.RecordError(1001)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, "signal_rooms_created_total") {
		t.Errorf("missing signal_rooms_created_total in:\n%s", body)
	}
	if !strings.Contains(body, "signal_rooms_live") {
		t.Errorf("missing signal_rooms_live in:\n%s", body)
	}
	if !strings.Contains(body, "signal_errors_total") {
		t.Errorf("missing signal_errors_total in:\n%s", body)
	}
}

func TestItoa(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"}, {1, "1"}, {42, "42"}, {-7, "-7"}, {12345, "12345"},
	}
	for _, c := range cases {
		if got := itoa(c.in); got != c.want {
			t.Errorf("itoa(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUptime(t *testing.T) {
	m := New()
	if m.Uptime() < 0 {
		t.Errorf("uptime negative")
	}
}
