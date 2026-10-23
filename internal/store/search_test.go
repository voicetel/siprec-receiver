package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

func seedSearchCall(t *testing.T, s *Store, callID, from, to, amd, disp string, started time.Time) {
	t.Helper()
	ctx := context.Background()

	if err := s.RecordLeg(ctx, siprecsrv.FinalizedLeg{
		CallID: callID, WAVPath: "/rec/" + callID + ".wav",
		DurationSec: 10, StartedAt: started,
	}); err != nil {
		t.Fatalf("RecordLeg %s: %v", callID, err)
	}

	if _, _, err := s.ApplyAMD(ctx, AMDNotification{
		SipCallID: callID, AnsweredBy: amd, From: from, To: to,
	}); err != nil {
		t.Fatalf("ApplyAMD %s: %v", callID, err)
	}

	if _, _, err := s.ApplyDisposition(ctx, DispositionNotification{
		SipCallID: callID, CallStatus: disp,
	}); err != nil {
		t.Fatalf("ApplyDisposition %s: %v", callID, err)
	}
}

// The recordings search must find calls independently by caller,
// called number, start date, final disposition, and AMD disposition.
func TestListCalls_SearchByListingFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seedSearchCall(t, s, "search-a", "+12125550100", "+12125550200",
		"human", "completed", time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC))
	seedSearchCall(t, s, "search-b", "+13135550100", "+13135550200",
		"machine_start", "busy", time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	seedSearchCall(t, s, "search-c", "+14145550100", "+15155550200",
		"human", "failed", time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))

	cases := []struct {
		name   string
		filter ListFilter
		want   int
	}{
		{"caller", ListFilter{From: "3135"}, 1},
		{"called", ListFilter{To: "5155"}, 1},
		{"general disposition text", ListFilter{Query: "busy"}, 1},
		{"AMD exact", ListFilter{AMD: "human"}, 2},
		{"disposition exact", ListFilter{Disposition: "failed"}, 1},
		{"date range", ListFilter{
			StartedAfter:  time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC),
			StartedBefore: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		}, 1},
		{"combined caller and AMD", ListFilter{From: "4145", AMD: "human"}, 1},
		{"combined mismatch", ListFilter{From: "4145", AMD: "machine_start"}, 0},
	}

	for _, tc := range cases {
		calls, total, err := s.ListCalls(ctx, tc.filter)
		if err != nil {
			t.Fatalf("%s: ListCalls: %v", tc.name, err)
		}

		if total != tc.want || len(calls) != tc.want {
			t.Errorf("%s: matched %d (total %d), want %d", tc.name, len(calls), total, tc.want)
		}

		for _, call := range calls {
			// Search results retain every field the listing promises.
			if call.StartedAt.IsZero() || call.FromNumber == "" || call.ToNumber == "" ||
				call.DispStatus == "" || call.AMDAnsweredBy == "" {
				t.Errorf("%s: result is missing a retained listing field: %+v", tc.name, call)
			}
		}
	}
}

// The list's exact-match filters are served by an index, not a scan of
// every call.
func TestFilterQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)

	for column, index := range map[string]string{
		"disp_status":     "idx_calls_disp_status",
		"amd_answered_by": "idx_calls_amd_answered_by",
	} {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM calls WHERE 1=1 AND `+column+` = ?`, "busy")
		if err != nil {
			t.Fatal(err)
		}

		var plan strings.Builder

		for rows.Next() {
			var id, parent, notUsed int
			var detail string

			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatal(err)
			}

			plan.WriteString(detail + "\n")
		}

		_ = rows.Close()

		if !strings.Contains(plan.String(), index) {
			t.Errorf("filter on %s does not use %s:\n%s", column, index, plan.String())
		}
	}
}
