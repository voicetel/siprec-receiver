package voiceml

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const (
	testAccount = "AC00000000000000000000000000000001"
	testCall    = "CA00000000000000000000000000000002"
)

// SIDs reach the request path, and come from notify payloads and SIP URI
// parameters: anything but AC/CA + 32 hex is refused before a request
// carrying the account's key is made (a crafted CallSid used to walk the
// path to any endpoint of the account).
func TestFetchCall_RefusesNonSIDs(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)

	for _, tc := range []struct{ account, call string }{
		{testAccount, "../../../Accounts/" + testAccount + "/Recordings.json?PageSize=1000#"},
		{testAccount, "CA0000000000000000000000000000000g"},
		{testAccount, "CA000"},
		{"AC/../x", testCall},
		{"XY00000000000000000000000000000001", testCall},
	} {
		_, _, err := New(srv.URL).FetchCall(context.Background(), tc.account, "k", tc.call)
		if !errors.Is(err, ErrBadSid) {
			t.Errorf("FetchCall(%q, %q) err = %v, want ErrBadSid", tc.account, tc.call, err)
		}
	}

	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was called %d times for refused SIDs", n)
	}
}

// A redirect is not followed: it would carry the account's API key to
// wherever it points (another host, or plain http).
func TestFetchCall_DoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Bool

	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	t.Cleanup(elsewhere.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, _, err := New(srv.URL).FetchCall(context.Background(), testAccount, "secret", testCall)

	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusFound {
		t.Fatalf("err = %v, want StatusError 302", err)
	}

	if leaked.Load() {
		t.Error("the API key followed the redirect")
	}
}

func TestValidSid(t *testing.T) {
	for s, want := range map[string]bool{
		"CA0123456789abcdefABCDEF0123456789": true,
		"CA0123456789abcdef0123456789abcde":  false, // 31 hex
		"ca0123456789abcdef0123456789abcdef": false, // prefix is case-sensitive
		"CA0123456789abcdef0123456789abcdeg": false,
	} {
		if got := validSid("CA", s); got != want {
			t.Errorf("validSid(CA, %q) = %v, want %v", s, got, want)
		}
	}
}
