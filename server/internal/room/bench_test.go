package room

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kku1993/simple-webrtc-server/internal/protocol"
)

// benchSignalRaw builds n `signal` messages of roughly sizeKB, each carrying a
// JSON-stringified SDP blob the way a browser sends one: the payload is itself
// JSON, so almost every byte of it is an escaped quote or backslash. That
// escaping is what makes decode-and-re-encode expensive, so a benchmark with a
// plain-text payload would measure the wrong thing.
func benchSignalRaw(n, sizeKB int) [][]byte {
	blob := `{"type":"offer","sdp":"` +
		strings.Repeat(`a=candidate:0 1 UDP 2130706431 192.168.1.1 5000 typ host\r\n`, (sizeKB*1024)/60) +
		`"}`
	out := make([][]byte, n)
	for i := range out {
		data, _ := json.Marshal(blob)
		out[i] = []byte(fmt.Sprintf(`{"type":"signal","seq":%d,"data":%s}`, i+1, data))
	}
	return out
}

// BenchmarkSignalRelay measures one relayed signal end to end through the room
// layer: decode the inbound message, relay it to the peer, encode the
// signal-response. The messages are decoded from bytes rather than built as
// structs on purpose -- the decode side is half of what this path costs, and
// the byte form is the same across implementations, so the benchmark is
// comparable against a build that decodes the payload into a string.
func BenchmarkSignalRelay(b *testing.B) {
	for _, sizeKB := range []int{1, 2, 8} {
		b.Run(fmt.Sprintf("sdp=%dKB", sizeKB), func(b *testing.B) {
			r, _ := testRegistry(b)
			host := NewSession(newFakeConn("1.1.1.1"))
			guest := NewSession(newFakeConn("2.2.2.2"))
			r.CreateRoom(host, protocol.CreateRoomMsg{Type: protocol.TypeCreateRoom, HostEpoch: "h"}, true)
			var roomID string
			r.mu.Lock()
			for id := range r.rooms {
				roomID = id
			}
			r.mu.Unlock()
			r.JoinRoom(guest, protocol.JoinRoomMsg{Type: protocol.TypeJoinRoom, RoomID: roomID, GuestEpoch: "g"})

			const batch = 64
			msgs := benchSignalRaw(batch, sizeKB)
			room := host.room
			gc := fc(guest)

			b.SetBytes(int64(len(msgs[0])))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%batch == 0 {
					// Duplicate suppression drops seq <= lastSeq, so rewind the
					// slot rather than grow an unbounded message set.
					b.StopTimer()
					room.mu.Lock()
					room.slots[protocol.RoleHost].lastSeq = 0
					room.mu.Unlock()
					gc.mu.Lock()
					gc.sent = nil
					gc.mu.Unlock()
					b.StartTimer()
				}
				var m protocol.SignalMsg
				if err := json.Unmarshal(msgs[i%batch], &m); err != nil {
					b.Fatalf("unmarshal: %v", err)
				}
				r.Signal(host, m)
			}
		})
	}
}
