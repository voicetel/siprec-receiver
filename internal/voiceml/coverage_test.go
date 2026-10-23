package voiceml

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestNewTrimsBaseURL(t *testing.T) {
	c := New("  https://cb.example.com///  ")
	if c.BaseURL != "https://cb.example.com" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}

	if c.HTTP == nil || c.HTTP.Timeout != 15*time.Second {
		t.Errorf("HTTP client = %+v, want 15s timeout", c.HTTP)
	}
}

func TestFetchCall_MissingArgs(t *testing.T) {
	c := New("http://127.0.0.1:1")
	ctx := context.Background()

	cases := [][3]string{
		{"", "k", "CA00000000000000000000000000000002"},
		{"AC00000000000000000000000000000001", "", "CA00000000000000000000000000000002"},
		{"AC00000000000000000000000000000001", "k", ""},
	}

	for _, a := range cases {
		if _, _, err := c.FetchCall(ctx, a[0], a[1], a[2]); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("FetchCall(%q) err = %v, want ErrNotConfigured", a, err)
		}
	}
}

func TestFetchCall_BadBaseURL(t *testing.T) {
	c := New("http://bad host\x7f")

	_, _, err := c.FetchCall(context.Background(), "AC00000000000000000000000000000001", "k", "CA00000000000000000000000000000002")
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("err = %v, want build request error", err)
	}
}

func TestFetchCall_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, _, err := New(url).FetchCall(context.Background(), "AC00000000000000000000000000000001", "secretkey", "CA00000000000000000000000000000002")
	if err == nil || !strings.Contains(err.Error(), "fetch call CA00000000000000000000000000000002") {
		t.Fatalf("err = %v, want fetch error", err)
	}

	if strings.Contains(err.Error(), "secretkey") {
		t.Errorf("error leaks api key: %v", err)
	}
}

func TestFetchCall_TruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sid":`))

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer cannot hijack")

			return
		}

		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)

			return
		}

		_ = buf.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	_, _, err := New(srv.URL).FetchCall(context.Background(), "AC00000000000000000000000000000001", "k", "CA00000000000000000000000000000002")
	if err == nil || !strings.Contains(err.Error(), "read response") {
		t.Fatalf("err = %v, want read response error", err)
	}
}

func TestFetchCall_InvalidJSON(t *testing.T) {
	srv := jsonServer(t, `not json`)

	_, _, err := New(srv.URL).FetchCall(context.Background(), "AC00000000000000000000000000000001", "k", "CA00000000000000000000000000000002")
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v, want decode response error", err)
	}
}

func TestFetchCall_WrongFieldType(t *testing.T) {
	srv := jsonServer(t, `{"sid": 42}`)

	_, _, err := New(srv.URL).FetchCall(context.Background(), "AC00000000000000000000000000000001", "k", "CA00000000000000000000000000000002")
	if err == nil || !strings.Contains(err.Error(), "decode call") {
		t.Fatalf("err = %v, want decode call error", err)
	}
}

func TestFetchCall_FillsMissingIdentifiers(t *testing.T) {
	srv := jsonServer(t, `{"status":"busy","duration":null,"parent_call_sid":null}`)

	rec, pretty, err := New(srv.URL).FetchCall(context.Background(), "ACffffffffffffffffffffffffffffffff", "k", "CAffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("FetchCall: %v", err)
	}

	if rec.Sid != "CAffffffffffffffffffffffffffffffff" || rec.AccountSid != "ACffffffffffffffffffffffffffffffff" || rec.Status != "busy" || rec.DurationSec != 0 || rec.ParentCallSid != "" {
		t.Errorf("record = %+v", rec)
	}

	if pretty != "{\n  \"duration\": null,\n  \"parent_call_sid\": null,\n  \"status\": \"busy\"\n}" {
		t.Errorf("pretty = %q", pretty)
	}
}

func TestFetchCall_ContextCanceled(t *testing.T) {
	srv := jsonServer(t, `{}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := New(srv.URL).FetchCall(ctx, "AC00000000000000000000000000000001", "k", "CA00000000000000000000000000000002")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStatusErrorMessage(t *testing.T) {
	if got := (&StatusError{Code: 503}).Error(); got != "VoiceML answered HTTP 503" {
		t.Errorf("Error() = %q", got)
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 0},
		{"null", 0},
		{`"62"`, 62},
		{`" 7 "`, 7},
		{`"abc"`, 0},
		{`""`, 0},
		{"45", 45},
		{"12.9", 12},
		{"true", 0},
		{`{"s":1}`, 0},
	}

	for _, tc := range cases {
		if got := parseDuration(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("parseDuration(%s) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
