package store

import (
	"context"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// A leg the SRS marks as never having received audio keeps the mark,
// and its call says so.
func TestNoAudioLegIsMarked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: "dlg-quiet", Label: "a", WAVPath: "/rec/quiet.wav", StartedAt: time.Now(), NoAudio: true,
	}); err != nil {
		t.Fatal(err)
	}

	calls, _, err := s.ListCalls(ctx, ListFilter{})
	if err != nil || len(calls) != 1 || !calls[0].HasNoAudio {
		t.Fatalf("list = %+v, %v", calls, err)
	}

	c, _ := s.GetCall(ctx, calls[0].ID)
	if len(c.Legs) != 1 || !c.Legs[0].NoAudio {
		t.Fatalf("legs = %+v", c.Legs)
	}

	if l, _ := s.GetLeg(ctx, c.Legs[0].ID); l == nil || !l.NoAudio {
		t.Errorf("GetLeg = %+v", l)
	}

	recordSimpleLeg(t, s, "dlg-loud", time.Now())

	if calls, _, _ := s.ListCalls(ctx, ListFilter{Query: "dlg-loud"}); len(calls) != 1 || calls[0].HasNoAudio {
		t.Errorf("a heard call is marked: %+v", calls)
	}
}
