// Package voiceml fetches Call records from a VoiceML
// deployment's Twilio-compatible REST API, on request, so a stored
// SIPREC recording can be enriched with the full calling record
// (final status, billable duration, price, answered_by, …).
//
// The fetch is GET /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json
// with HTTP Basic auth (AccountSid as username, the account's API key
// as password) — exactly how Twilio SDKs authenticate. Keys are
// per-account and come from the store (internal/store VoiceMLAccount);
// this package only knows how to make the call.
package voiceml

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/voicetel/siprec-receiver/internal/store"
)

// Client fetches from one VoiceML base URL.
type Client struct {
	BaseURL string // e.g. https://cb.example.com (no trailing slash)
	HTTP    *http.Client
}

// New builds a Client with a bounded HTTP timeout. It follows no
// redirects: the request carries the account's API key, and a redirect
// (to another host, or from https to http) must not take it along. A
// redirect answer surfaces as a StatusError.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTP: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ErrNotConfigured reports a missing base URL or credential.
var ErrNotConfigured = errors.New("VoiceML fetch is not configured")

// ErrBadSid reports an AccountSid or CallSid that is not a VoiceML SID.
// Both reach the request path, and they come from notify payloads and
// SIP URI parameters, so anything but "AC"/"CA" + 32 hex is refused
// before a request (carrying the account's key) is made.
var ErrBadSid = errors.New("not a VoiceML SID")

// validSid reports whether s is prefix + 32 hex digits.
func validSid(prefix, s string) bool {
	if len(s) != len(prefix)+32 || !strings.HasPrefix(s, prefix) {
		return false
	}

	for _, c := range s[len(prefix):] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}

	return true
}

// StatusError is a non-200 from VoiceML. The body is
// deliberately not carried: VoiceML error text can echo the
// request path, including the AccountSid.
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("VoiceML answered HTTP %d", e.Code)
}

// callJSON is the subset of the Twilio-shaped Call resource we
// denormalize. Duration is a string in this JSON shape; StartTime /
// EndTime are RFC 2822 strings we keep verbatim in the stored JSON
// but do not parse.
type callJSON struct {
	Sid        string          `json:"sid"`
	AccountSid string          `json:"account_sid"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Status     string          `json:"status"`
	Direction  string          `json:"direction"`
	AnsweredBy string          `json:"answered_by"`
	Duration   json.RawMessage `json:"duration"`
}

// FetchCall GETs one Call record. It returns the denormalized subset
// plus the record pretty-printed for storage/display. apiKey is used
// only for the Authorization header and never logged or returned.
func (c *Client) FetchCall(ctx context.Context, accountSid, apiKey, callSid string) (store.VoiceMLRecord, string, error) {
	if c.BaseURL == "" || accountSid == "" || apiKey == "" || callSid == "" {
		return store.VoiceMLRecord{}, "", ErrNotConfigured
	}

	if !validSid("AC", accountSid) {
		return store.VoiceMLRecord{}, "", fmt.Errorf("voiceml: AccountSid %q: %w", accountSid, ErrBadSid)
	}

	if !validSid("CA", callSid) {
		return store.VoiceMLRecord{}, "", fmt.Errorf("voiceml: CallSid %q: %w", callSid, ErrBadSid)
	}

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Calls/%s.json",
		c.BaseURL, url.PathEscape(accountSid), url.PathEscape(callSid))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return store.VoiceMLRecord{}, "", fmt.Errorf("voiceml: build request: %w", err)
	}

	req.SetBasicAuth(accountSid, apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return store.VoiceMLRecord{}, "", fmt.Errorf("voiceml: fetch call %s: %w", callSid, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return store.VoiceMLRecord{}, "", fmt.Errorf("voiceml: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return store.VoiceMLRecord{}, "", &StatusError{Code: resp.StatusCode}
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return store.VoiceMLRecord{}, "", fmt.Errorf("voiceml: decode response: %w", err)
	}

	var parsed callJSON
	if err := json.Unmarshal(body, &parsed); err != nil {
		return store.VoiceMLRecord{}, "", fmt.Errorf("voiceml: decode call: %w", err)
	}

	pretty, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		pretty = body
	}

	rec := store.VoiceMLRecord{
		Sid:         parsed.Sid,
		AccountSid:  parsed.AccountSid,
		From:        parsed.From,
		To:          parsed.To,
		Status:      parsed.Status,
		Direction:   parsed.Direction,
		AnsweredBy:  parsed.AnsweredBy,
		DurationSec: parseDuration(parsed.Duration),
	}

	if rec.Sid == "" {
		rec.Sid = callSid
	}

	if rec.AccountSid == "" {
		rec.AccountSid = accountSid
	}

	return rec, string(pretty), nil
}

// parseDuration accepts Twilio's string seconds ("12"), a bare JSON
// number, or null/absent.
func parseDuration(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(s))

		return n
	}

	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int(f)
	}

	return 0
}
