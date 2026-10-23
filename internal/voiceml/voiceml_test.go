package voiceml

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchCall_TwilioShape(t *testing.T) {
	var gotPath, gotUser, gotPass string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"sid": "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"account_sid": "AC00000000000000000000000000000001",
			"from": "+12165285911", "to": "+12085179911",
			"status": "completed", "direction": "outbound-api",
			"answered_by": "machine_end_beep", "duration": "62",
			"price": "-0.02000", "price_unit": "USD",
			"parent_call_sid": "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}`))
	}))
	defer srv.Close()

	rec, pretty, err := New(srv.URL).FetchCall(context.Background(),
		"AC00000000000000000000000000000001", "key123", "CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FetchCall: %v", err)
	}

	wantPath := "/2010-04-01/Accounts/AC00000000000000000000000000000001/Calls/CAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}

	if gotUser != "AC00000000000000000000000000000001" || gotPass != "key123" {
		t.Errorf("basic auth = %q/%q", gotUser, gotPass)
	}

	if rec.Status != "completed" || rec.DurationSec != 62 ||
		rec.AnsweredBy != "machine_end_beep" || rec.From != "+12165285911" ||
		rec.ParentCallSid != "CAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("record = %+v", rec)
	}

	if !strings.Contains(pretty, `"answered_by"`) {
		t.Errorf("pretty JSON missing fields:\n%s", pretty)
	}
}

func TestFetchCall_StatusErrorAndNotConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":20404,"message":"not found /2010-04-01/Accounts/ACtest/..."}`))
	}))
	defer srv.Close()

	_, _, err := New(srv.URL).FetchCall(context.Background(), "AC00000000000000000000000000000001", "k", "CA00000000000000000000000000000002")
	var stErr *StatusError
	if !errors.As(err, &stErr) || stErr.Code != 404 {
		t.Fatalf("err = %v, want StatusError 404", err)
	}

	if strings.Contains(err.Error(), "AC00000000000000000000000000000001") {
		t.Errorf("error leaks account sid: %v", err)
	}

	if _, _, err := New("").FetchCall(context.Background(), "AC00000000000000000000000000000001", "k", "CA00000000000000000000000000000002"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("empty base URL err = %v, want ErrNotConfigured", err)
	}
}
