package store

import (
	"context"
	"testing"
	"time"
)

func TestDispositionOrdering(t *testing.T) {
	for _, mode := range []string{"sequence", "timestamp", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			post := func(status string, order int64, duration int) int64 {
				n := DispositionNotification{CallSid: "CAorder", CallStatus: status, DurationSec: duration, SipCode: 200}
				if mode == "sequence" {
					n.SequenceNumber = &order
				}
				if mode == "timestamp" {
					n.Timestamp = time.Unix(order, 0)
				}
				id, _, err := s.ApplyDisposition(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			post("queued", 0, 0)
			post("ringing", 1, 0)
			id := post("completed", 3, 62)
			post("ringing", 2, 0)
			post("completed", 3, 0) // duplicate delivery must be idempotent
			post("failed", 2, 0)    // stale terminal event must not replace a newer one
			c, err := s.GetCall(ctx, id)
			if err != nil || c.DispStatus != "completed" || c.DispDurationSec != 62 {
				t.Fatalf("outcome after late callbacks: %+v, %v", c, err)
			}
		})
	}
}
