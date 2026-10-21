package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDispositionOrderParsing(t *testing.T) {
	for _, fields := range []map[string]string{
		{}, {"SequenceNumber": "0"}, {"Timestamp": "2026-10-21T12:00:00.123Z"},
		{"Timestamp": "Wed, 21 Oct 2026 12:00:00 +0000"},
		{"Timestamp": "Wed, 21 Oct 2026 12:00:00 GMT"},
	} {
		seq, at, err := parseDispositionOrder(fields)
		if err != nil {
			t.Fatal(err)
		}
		if fields["SequenceNumber"] != "" && (seq == nil || *seq != 0) {
			t.Fatal("sequence zero was not retained")
		}
		if fields["Timestamp"] != "" && at.Year() != 2026 {
			t.Fatal("timestamp not parsed")
		}
	}
	for _, fields := range []map[string]string{{"SequenceNumber": "-1"}, {"SequenceNumber": "bad"}, {"Timestamp": "bad"}} {
		if _, _, err := parseDispositionOrder(fields); err == nil {
			t.Fatalf("accepted invalid ordering: %v", fields)
		}
	}
}

func TestNotifyDispositionIgnoresLateEvents(t *testing.T) {
	f := newFixture(t, adminEmail)
	id := seedCallWithIDs(t, f)
	for _, event := range []struct{ status, seq, duration string }{
		{"completed", "3", "62"}, {"ringing", "2", "0"}, {"completed", "3", "0"},
	} {
		form := url.Values{"CallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "CallStatus": {event.status}, "SequenceNumber": {event.seq}, "CallDuration": {event.duration}, "Timestamp": {time.Now().UTC().Format(time.RFC3339Nano)}}
		req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/api/notify/disposition", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status: %d", res.StatusCode)
		}
	}
	c, err := f.store.GetCall(context.Background(), id)
	if err != nil || c.DispStatus != "completed" || c.DispDurationSec != 62 {
		t.Fatalf("outcome: %+v, %v", c, err)
	}
}
