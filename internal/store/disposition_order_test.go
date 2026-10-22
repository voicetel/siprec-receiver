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

// Timestamps have one-second precision: a terminal event sharing its
// second with the ringing event before it must still land, and a
// nonterminal one sharing a terminal event's second must not replace it.
func TestDispositionTimestampTie(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Unix(1_800_000_000, 0)

	apply := func(status string) *Call {
		t.Helper()

		id, _, err := s.ApplyDisposition(ctx, DispositionNotification{CallSid: "CAtie", CallStatus: status, Timestamp: at})
		if err != nil {
			t.Fatal(err)
		}

		c, _ := s.GetCall(ctx, id)

		return c
	}

	apply("ringing")

	if c := apply("busy"); c.DispStatus != "busy" {
		t.Fatalf("busy in the ringing event's second = %q, want busy", c.DispStatus)
	}

	if c := apply("ringing"); c.DispStatus != "busy" {
		t.Errorf("ringing in the busy event's second replaced it: %q", c.DispStatus)
	}

	if c := apply("completed"); c.DispStatus != "busy" {
		t.Errorf("a second terminal event in the same second replaced the first: %q", c.DispStatus)
	}
}
