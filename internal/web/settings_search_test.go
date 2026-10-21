package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
	"github.com/voicetel/siprec-receiver/internal/store"
)

func seedWebSearchCall(t *testing.T, f *fixture, callID, from, to, amd, disp string, started time.Time) {
	t.Helper()
	ctx := context.Background()

	if err := f.store.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: callID, WAVPath: filepath.Join(f.recDir, callID+".wav"),
		DurationSec: 10, StartedAt: started,
	}); err != nil {
		t.Fatalf("RecordLeg %s: %v", callID, err)
	}

	if _, _, err := f.store.ApplyAMD(ctx, store.AMDNotification{
		SipCallID: callID, AnsweredBy: amd, From: from, To: to,
	}); err != nil {
		t.Fatalf("ApplyAMD %s: %v", callID, err)
	}

	if _, _, err := f.store.ApplyDisposition(ctx, store.DispositionNotification{
		SipCallID: callID, CallStatus: disp,
	}); err != nil {
		t.Fatalf("ApplyDisposition %s: %v", callID, err)
	}
}

func getBody(t *testing.T, c *http.Client, rawURL string) (int, string) {
	t.Helper()

	resp, err := c.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}

	defer func() { _ = resp.Body.Close() }()

	b, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(b)
}

// Search results keep To, From, time, disposition, and AMD disposition.
func TestRecordingsSearchRetainsListingFields(t *testing.T) {
	f := newFixture(t, adminEmail)
	admin := f.loginClient(t)

	seedWebSearchCall(t, f, "web-search-a", "+12125550100", "+12125550200",
		"human", "completed", time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC))
	seedWebSearchCall(t, f, "web-search-b", "+13135550100", "+13135550200",
		"machine_start", "busy", time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	status, body := getBody(t, admin, f.srv.URL+
		"/recordings?from_number=3135&disposition=busy&amd=machine_start")
	body = strings.ReplaceAll(body, "&#43;", "+")
	if status != http.StatusOK {
		t.Fatalf("search status = %d", status)
	}

	for _, want := range []string{
		"Time", "From", "To", "Disposition", "AMD disposition",
		"+13135550100", "+13135550200", "busy", "machine_start", "2026-01-15",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search result is missing %q", want)
		}
	}

	if strings.Contains(body, "+12125550100") {
		t.Error("search returned the non-matching caller")
	}

	_, body = getBody(t, admin, f.srv.URL+
		"/recordings?start_date=2026-01-12&end_date=2026-01-31")
	body = strings.ReplaceAll(body, "&#43;", "+")
	if !strings.Contains(body, "+13135550100") || strings.Contains(body, "+12125550100") {
		t.Error("date search did not isolate the January 15 call")
	}
}

func TestAdminSettingsRetentionAndManualPurge(t *testing.T) {
	f := newFixture(t, adminEmail)
	admin := f.loginClient(t)

	status, body := getBody(t, admin, f.srv.URL+"/admin/settings")
	if status != http.StatusOK || !strings.Contains(body, "Recording retention") {
		t.Fatalf("settings page = %d, has form=%v", status, strings.Contains(body, "Recording retention"))
	}

	resp, err := admin.PostForm(f.srv.URL+"/admin/settings", url.Values{
		"retention_days": {"30"},
		"csrf_token":     {csrfFromPage(t, admin, f.srv.URL+"/admin/settings")},
	})
	if err != nil {
		t.Fatalf("save settings: %v", err)
	}
	_ = resp.Body.Close()

	days, fromUI, err := f.store.EffectiveRetentionDays(context.Background(), 0)
	if err != nil || days != 30 || !fromUI {
		t.Fatalf("effective retention = %d, fromUI=%v, err=%v", days, fromUI, err)
	}

	expiredPath := filepath.Join(f.recDir, "expired.wav")
	if err := os.WriteFile(expiredPath, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.store.RecordLeg(context.Background(), siprecsrv.FinalizedLeg{
		CallID: "expired-call", WAVPath: expiredPath, DurationSec: 1,
		StartedAt: time.Now().UTC().AddDate(0, 0, -40),
	}); err != nil {
		t.Fatalf("RecordLeg expired: %v", err)
	}

	resp2, err := admin.PostForm(f.srv.URL+"/admin/settings/purge", url.Values{
		"csrf_token": {csrfFromPage(t, admin, f.srv.URL+"/admin/settings")},
	})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	_ = resp2.Body.Close()

	if _, total, _ := f.store.ListCalls(context.Background(), store.ListFilter{}); total != 0 {
		t.Errorf("calls after manual purge = %d, want 0", total)
	}

	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Errorf("expired WAV still exists: %v", err)
	}

	status, body = getBody(t, admin, f.srv.URL+"/admin/audit")
	if status != http.StatusOK || !strings.Contains(body, "settings.retention") ||
		!strings.Contains(body, "maintenance.purge") {
		t.Error("settings/purge actions were not audit logged")
	}
}

func TestSettingsAreAdminOnly(t *testing.T) {
	f := newFixture(t, userEmail)

	if err := f.store.UpsertUser(context.Background(), store.User{
		Email: userEmail, Role: "user", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	user := f.loginClient(t)
	status, _ := getBody(t, user, f.srv.URL+"/admin/settings")

	if status != http.StatusForbidden {
		t.Fatalf("user GET /admin/settings = %d, want 403", status)
	}
}
