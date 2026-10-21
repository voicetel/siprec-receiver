package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

func TestEffectiveRetentionDays_UIWinsOnceSet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	days, fromUI, err := s.EffectiveRetentionDays(ctx, 30)
	if err != nil || days != 30 || fromUI {
		t.Fatalf("env default = %d, fromUI=%v, err=%v", days, fromUI, err)
	}

	if err := s.SetSetting(ctx, SettingRetentionDays, "14"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	days, fromUI, err = s.EffectiveRetentionDays(ctx, 30)
	if err != nil || days != 14 || !fromUI {
		t.Fatalf("stored setting = %d, fromUI=%v, err=%v", days, fromUI, err)
	}

	if err := s.SetSetting(ctx, SettingRetentionDays, "not-a-number"); err != nil {
		t.Fatalf("SetSetting invalid: %v", err)
	}

	days, fromUI, err = s.EffectiveRetentionDays(ctx, 30)
	if err != nil || days != 30 || fromUI {
		t.Fatalf("invalid stored setting should fall back to env: %d, fromUI=%v, err=%v", days, fromUI, err)
	}
}

func TestPurgeExpired_RemovesOldCallsAndFilesOnly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	oldPath := filepath.Join(dir, "old.wav")
	recentPath := filepath.Join(dir, "recent.wav")

	for _, path := range []string{oldPath, recentPath} {
		if err := os.WriteFile(path, []byte("RIFF"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	for _, tc := range []struct {
		callID  string
		path    string
		started time.Time
	}{
		{"dlg-old", oldPath, now.AddDate(0, 0, -40)},
		{"dlg-recent", recentPath, now},
	} {
		if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
			CallID: tc.callID, WAVPath: tc.path, DurationSec: 1, StartedAt: tc.started,
		}); err != nil {
			t.Fatalf("RecordLeg %s: %v", tc.callID, err)
		}
	}

	calls, files, err := s.PurgeExpired(ctx, 30)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}

	if calls != 1 || files != 1 {
		t.Fatalf("purged calls/files = %d/%d, want 1/1", calls, files)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old WAV still exists: %v", err)
	}

	if _, err := os.Stat(recentPath); err != nil {
		t.Errorf("recent WAV was removed: %v", err)
	}

	if _, total, _ := s.ListCalls(ctx, ListFilter{}); total != 1 {
		t.Errorf("calls after purge = %d, want 1", total)
	}

	at, lastCalls, lastFiles := s.LastPurge(ctx)
	if at.IsZero() || lastCalls != 1 || lastFiles != 1 {
		t.Errorf("last purge record = %v, %d, %d", at, lastCalls, lastFiles)
	}

	calls, files, err = s.PurgeExpired(ctx, 0)
	if err != nil || calls != 0 || files != 0 {
		t.Errorf("disabled purge = %d/%d, err=%v", calls, files, err)
	}
}

func TestDatabaseMaintenanceHelpers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	stats, err := s.DBStats(ctx)
	if err != nil {
		t.Fatalf("DBStats: %v", err)
	}

	if stats.PageCount <= 0 || stats.PageSize <= 0 || stats.ApproxBytes <= 0 {
		t.Errorf("unexpected DB stats: %+v", stats)
	}

	if err := s.Checkpoint(ctx); err != nil {
		t.Errorf("Checkpoint: %v", err)
	}

	if err := s.Optimize(ctx); err != nil {
		t.Errorf("Optimize: %v", err)
	}

	vacuumed, err := s.VacuumIfNeeded(ctx, 0.2)
	if err != nil {
		t.Errorf("VacuumIfNeeded: %v", err)
	}

	if vacuumed {
		t.Error("tiny test database should not be vacuumed")
	}
}
