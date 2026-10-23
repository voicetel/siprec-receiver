package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/voicetel/siprec-receiver/internal/store"
)

const dialChildSid = "CAcccccccccccccccccccccccccccccccc"

// A <Dial> child leg's StatusCallback (its own CallSid plus
// ParentCallSid) becomes its own call, linked both ways on the detail
// pages and badged in the list.
func TestDialLegCallbacksLinkToTheRecordedParent(t *testing.T) {
	e := newCovEnv(t, covOpts{notifySecret: secret})
	parent := seedCallWithIDs(t, e.fixture)
	bearer := map[string]string{"Authorization": "Bearer " + secret}

	form := url.Values{
		"CallSid": {dialChildSid}, "ParentCallSid": {"CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"CallStatus": {"completed"}, "CallDuration": {"31"}, "To": {"+15550001111"},
	}
	r := covNotify(t, e.srv.URL+"/api/notify/disposition", "application/x-www-form-urlencoded", form.Encode(), bearer)
	wantStatus(t, "child disposition", r, http.StatusOK)

	if !strings.Contains(r.body, `"matched":false`) {
		t.Fatalf("the child leg matched an existing call: %s", r.body)
	}

	if p, _ := e.store.GetCall(context.Background(), parent); p.DispStatus != "" {
		t.Errorf("the child's outcome landed on the parent: %q", p.DispStatus)
	}

	legs, err := e.store.DialLegs(context.Background(), "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || len(legs) != 1 {
		t.Fatalf("DialLegs = %+v, %v", legs, err)
	}

	child := legs[0].ID
	c := e.loginClient(t)

	r = covGet(t, c, e.srv.URL+"/recordings/"+strconv.FormatInt(parent, 10))
	if r.status != http.StatusOK || !strings.Contains(r.body, "Dialed legs") ||
		!strings.Contains(r.body, dialChildSid) || !strings.Contains(r.body, "/recordings/"+strconv.FormatInt(child, 10)) {
		t.Errorf("parent page = %d, missing the dialed leg:\n%s", r.status, r.body)
	}

	r = covGet(t, c, e.srv.URL+"/recordings/"+strconv.FormatInt(child, 10))
	if r.status != http.StatusOK || !strings.Contains(r.body, "Dialed by") ||
		!strings.Contains(r.body, `href="/recordings/`+strconv.FormatInt(parent, 10)+`"`) {
		t.Errorf("child page = %d, missing the parent link:\n%s", r.status, r.body)
	}

	if r = covGet(t, c, e.srv.URL+"/recordings"); !strings.Contains(r.body, "dialed leg") {
		t.Errorf("list does not badge the dialed leg:\n%s", r.body)
	}

	// The parent's page fails rather than hiding a leg it cannot read.
	e.execSQL(t, `UPDATE calls SET started_at = 'not a time' WHERE call_sid = '`+dialChildSid+`'`)
	wantStatus(t, "unreadable dial leg", covGet(t, c, e.srv.URL+"/recordings/"+strconv.FormatInt(parent, 10)),
		http.StatusInternalServerError)
}

// A JSON poster's snake_case parent_call_sid links the same way, and a
// parent that is not stored here shows unlinked.
func TestDialLegJSONParentNotStored(t *testing.T) {
	e := newCovEnv(t, covOpts{notifySecret: secret})

	r := covNotify(t, e.srv.URL+"/api/notify/amd", "application/json",
		`{"call_sid":"`+dialChildSid+`","parent_call_sid":"CAdddddddddddddddddddddddddddddddd","answered_by":"human"}`,
		map[string]string{"Authorization": "Bearer " + secret})
	wantStatus(t, "json amd", r, http.StatusOK)

	calls, _, err := e.store.ListCalls(context.Background(), store.ListFilter{Query: dialChildSid})
	if err != nil || len(calls) != 1 || calls[0].ParentCallSid != "CAdddddddddddddddddddddddddddddddd" {
		t.Fatalf("stored = %+v, %v", calls, err)
	}

	r = covGet(t, e.loginClient(t), e.srv.URL+"/recordings/"+strconv.FormatInt(calls[0].ID, 10))
	if !strings.Contains(r.body, "(not stored here)") {
		t.Errorf("unstored parent not marked:\n%s", r.body)
	}
}
