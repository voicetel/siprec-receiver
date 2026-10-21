package siprecsrv

import (
	"context"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/rtp"
)

func TestReaperDistinguishesPausedFromAbandoned(t *testing.T) {
	srv := New("127.0.0.1", t.TempDir(), rtp.NewPortPool(49100, 49110))
	t.Cleanup(srv.Shutdown)
	srv.shared.reaperTick = time.Millisecond
	paused := &session{callID: "paused", paused: true}
	paused.lastActivity.Store(time.Now().Add(-6 * time.Minute).UnixNano())
	expired := &session{callID: "expired", paused: true}
	expired.lastActivity.Store(time.Now().Add(-25 * time.Hour).UnixNano())
	active := &session{callID: "abandoned"}
	active.lastActivity.Store(time.Now().Add(-6 * time.Minute).UnixNano())
	srv.shared.sessions = map[string]*session{"paused": paused, "expired": expired, "abandoned": active}
	srv.shared.startReaper(context.Background())
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap := srv.SessionsSnapshot()
		if len(snap) == 1 {
			if snap[0].CallID != "paused" {
				t.Fatalf("surviving session: %+v", snap)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("reaper did not expire abandoned sessions")
}
