package web

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- Twilio's webhook signature is HMAC-SHA1 by definition
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/voicetel/siprec-receiver/internal/store"
)

// handleNotifyAMD is the call-notification endpoint: it logs the AMD
// (answering-machine detection) disposition for a call.
//
// It accepts the exact form field names VoiceML sends to an
// AsyncAmdStatusCallback URL (CallSid, AccountSid, AnsweredBy,
// MachineDetectionDuration, From, To, CallStatus, Direction, Caller,
// Called, ApiVersion) as a form-encoded or JSON POST, or as a GET's
// query string (a callback whose method is GET), plus the SIPREC
// correlation keys a poster that knows them can add:
//
//	SipCallId — the SIPREC recording dialog's SIP Call-ID
//	FsUuid    — the FreeSWITCH parent call UUID (the RS-Metadata
//	            session_id, base64-decoded, identifies the same call)
//	CallSid   — matched against a recording whose INVITE Request-URI
//	            carried ;vm-callsid=CA…
//
// ParentCallSid, which rides a <Dial> child leg's callbacks, is not a
// correlation key: the child is its own call, linked to its parent.
//
// Auth is a shared secret (NOTIFY_SECRET): Authorization: Bearer,
// X-Notify-Secret, or ?secret= (the query form exists for webhook
// senders like VoiceML's that cannot set custom headers). A VoiceML
// callback can instead prove itself with its X-Twilio-Signature, made
// with the API key stored for its AccountSid under Integrations; such a
// notification may only touch that account's calls.
// When NOTIFY_SECRET is unset the endpoint is disabled (503) rather
// than open.
func (s *Server) handleNotifyAMD(w http.ResponseWriter, r *http.Request) {
	req, ok := s.readNotify(w, r)
	if !ok {
		return
	}

	fields := req.fields

	answeredBy := fields["AnsweredBy"]
	if answeredBy == "" {
		http.Error(w, "AnsweredBy is required", http.StatusBadRequest)

		return
	}

	durationMs, _ := strconv.Atoi(fields["MachineDetectionDuration"])

	n := store.AMDNotification{
		CallSid:    fields["CallSid"],
		AccountSid: fields["AccountSid"],
		AnsweredBy: answeredBy,
		DurationMs: durationMs,
		From:       firstNonEmpty(fields["From"], fields["Caller"]),
		To:         firstNonEmpty(fields["To"], fields["Called"]),
		CallStatus: fields["CallStatus"],
		Direction:  fields["Direction"],
		SipCallID:  firstNonEmpty(fields["SipCallId"], fields["SipCallID"]),
		FsUUID:     fields["FsUuid"],
		Raw:        req.raw,

		ParentCallSid: fields["ParentCallSid"],
		AuthAccount:   req.account,
	}

	id, matched, err := s.Store.ApplyAMD(r.Context(), n)
	if notifyFailed(w, "amd", n.CallSid, n.SipCallID, err) {
		return
	}

	log.Printf("[notify] amd logged id=%d matched=%v answeredBy=%q CallSid=%q",
		id, matched, n.AnsweredBy, n.CallSid)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "matched": matched})
}

// handleNotifyDisposition is the final-outcome companion to
// handleNotifyAMD: /api/notify/disposition logs what ultimately
// happened to the call (the StatusCallback "completed" event's
// content). Same auth, body shapes, and correlation keys as the AMD
// endpoint; CallStatus is required instead of AnsweredBy. A
// disposition carrying AnsweredBy also fills still-empty AMD fields.
func (s *Server) handleNotifyDisposition(w http.ResponseWriter, r *http.Request) {
	req, ok := s.readNotify(w, r)
	if !ok {
		return
	}

	fields := req.fields

	if fields["CallStatus"] == "" {
		http.Error(w, "CallStatus is required", http.StatusBadRequest)

		return
	}

	// CallDuration is seconds. Twilio-shaped senders' Duration is the
	// call rounded up to whole minutes, so it is not a fallback.
	durationSec, _ := strconv.Atoi(fields["CallDuration"])
	sipCode, _ := strconv.Atoi(fields["SipResponseCode"])
	seq, eventTime := parseDispositionOrder(fields)

	n := store.DispositionNotification{
		SequenceNumber: seq,
		Timestamp:      eventTime,
		CallSid:        fields["CallSid"],
		AccountSid:     fields["AccountSid"],
		CallStatus:     fields["CallStatus"],
		DurationSec:    durationSec,
		SipCode:        sipCode,
		ErrorCode:      fields["ErrorCode"],
		ErrorMessage:   fields["ErrorMessage"],
		AnsweredBy:     fields["AnsweredBy"],
		From:           firstNonEmpty(fields["From"], fields["Caller"]),
		To:             firstNonEmpty(fields["To"], fields["Called"]),
		Direction:      fields["Direction"],
		SipCallID:      firstNonEmpty(fields["SipCallId"], fields["SipCallID"]),
		FsUUID:         fields["FsUuid"],
		ParentCallSid:  fields["ParentCallSid"],
		Raw:            req.raw,
		AuthAccount:    req.account,
	}

	id, matched, err := s.Store.ApplyDisposition(r.Context(), n)
	if notifyFailed(w, "disposition", n.CallSid, n.SipCallID, err) {
		return
	}

	log.Printf("[notify] disposition logged id=%d matched=%v status=%q CallSid=%q",
		id, matched, n.CallStatus, n.CallSid)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "matched": matched})
}

// notifyRequest is an authorized notification's parsed payload.
type notifyRequest struct {
	fields map[string]string
	raw    string
	// account is set when the AccountSid's webhook signature, not the
	// shared secret, authorized the request.
	account string
}

// readNotify runs what both notify endpoints share: enabled, body read
// and parsed, authorized. On false the response has been written.
func (s *Server) readNotify(w http.ResponseWriter, r *http.Request) (notifyRequest, bool) {
	if s.NotifySecret == "" {
		http.Error(w, "notify endpoint disabled (NOTIFY_SECRET not set)", http.StatusServiceUnavailable)

		return notifyRequest{}, false
	}

	secretOK := s.notifyAuthorized(r)
	if !secretOK && r.Header.Get(signatureHeader) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return notifyRequest{}, false
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)

		return notifyRequest{}, false
	}

	fields, raw := parseNotifyBody(r, body)
	if secretOK {
		return notifyRequest{fields: fields, raw: raw}, true
	}

	account, err := s.signedBy(r, body, fields["AccountSid"])
	if err != nil {
		log.Printf("[notify] signature key lookup (AccountSid=%q): %v", fields["AccountSid"], err)
		http.Error(w, "failed to verify the signature", http.StatusInternalServerError)

		return notifyRequest{}, false
	}

	if account == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return notifyRequest{}, false
	}

	return notifyRequest{fields: fields, raw: raw, account: account}, true
}

// notifyFailed answers a failed Apply* and reports whether it did.
func notifyFailed(w http.ResponseWriter, kind, callSid, sipCallID string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNoKey):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrForeignAccount):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		log.Printf("[notify] apply %s (CallSid=%q SipCallId=%q): %v", kind, callSid, sipCallID, err)
		http.Error(w, "failed to log disposition", http.StatusInternalServerError)
	}

	return true
}

func (s *Server) notifyAuthorized(r *http.Request) bool {
	if s.querySecret(r) {
		// The query form exists for webhook senders that cannot set
		// headers, but URLs land in proxy/access logs — say so once.
		s.secretQueryWarn.Do(func() {
			log.Printf("[notify] WARNING: notify secret presented as ?secret= query parameter; " +
				"it will appear in request logs. Prefer the Authorization: Bearer header, " +
				"or let VoiceML's X-Twilio-Signature authorize its callbacks.")
		})

		return true
	}

	candidates := []string{}

	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		candidates = append(candidates, strings.TrimPrefix(h, "Bearer "))
	}

	if h := r.Header.Get("X-Notify-Secret"); h != "" {
		candidates = append(candidates, h)
	}

	for _, c := range candidates {
		if c != "" && hmac.Equal([]byte(c), []byte(s.NotifySecret)) {
			return true
		}
	}

	return false
}

// querySecret reports whether ?secret= carries the secret, decoded or
// as typed: a "+" pasted into a callback URL unencoded decodes as a
// space.
func (s *Server) querySecret(r *http.Request) bool {
	values := []string{r.URL.Query().Get("secret")}

	for _, kv := range strings.Split(r.URL.RawQuery, "&") {
		if raw, ok := strings.CutPrefix(kv, "secret="); ok {
			values = append(values, raw)
		}
	}

	for _, v := range values {
		if v != "" && hmac.Equal([]byte(v), []byte(s.NotifySecret)) {
			return true
		}
	}

	return false
}

// signatureHeader is the header Twilio-compatible senders (VoiceML's
// included) sign their webhooks with.
const signatureHeader = "X-Twilio-Signature"

// signedBy returns accountSid when r carries a valid webhook signature
// made with that account's stored API key, else "". The signed URL is
// the one the sender was configured with: PUBLIC_BASE_URL plus the
// request URI.
func (s *Server) signedBy(r *http.Request, body []byte, accountSid string) (string, error) {
	if accountSid == "" || s.PublicBaseURL == "" {
		return "", nil
	}

	acct, err := s.Store.GetVoiceMLAccount(r.Context(), accountSid)
	if err != nil || acct == nil || acct.APIKey == "" {
		return "", err
	}

	// A POST signs its form fields; a GET's are already in the URL.
	// JSON bodies are not signed this way.
	var params url.Values

	if r.Method == http.MethodPost {
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			return "", nil
		}

		params, _ = url.ParseQuery(string(body))
	}

	got := []byte(r.Header.Get(signatureHeader))

	for _, u := range signedURLs(s.PublicBaseURL + r.URL.RequestURI()) {
		if hmac.Equal(got, []byte(webhookSignature(acct.APIKey, u, params))) {
			return accountSid, nil
		}
	}

	return "", nil
}

// webhookSignature is Twilio's X-Twilio-Signature: the base64
// HMAC-SHA1, keyed with the account's API key, of the URL followed by
// every form field's name and value in name order.
func webhookSignature(key, rawURL string, params url.Values) string {
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}

	sort.Strings(names)

	var b strings.Builder

	b.WriteString(rawURL)

	for _, name := range names {
		for _, v := range params[name] {
			b.WriteString(name)
			b.WriteString(v)
		}
	}

	mac := hmac.New(sha1.New, []byte(key))
	_, _ = mac.Write([]byte(b.String()))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signedURLs are the forms of rawURL a sender may have signed: as is,
// and with the scheme's default port added or removed (senders and
// proxies differ on whether ":443" is part of the URL).
func signedURLs(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return []string{rawURL}
	}

	defaultPort := map[string]string{"http": "80", "https": "443"}[u.Scheme]
	if defaultPort == "" {
		return []string{rawURL}
	}

	alt := *u

	switch u.Port() {
	case "":
		alt.Host = net.JoinHostPort(u.Hostname(), defaultPort)
	case defaultPort:
		alt.Host = strings.TrimSuffix(u.Host, ":"+defaultPort)
	default:
		return []string{rawURL}
	}

	return []string{rawURL, alt.String()}
}

// parseNotifyBody accepts JSON or form-encoding (VoiceML/Twilio
// webhooks are form-encoded), or for GET the query string, and returns
// a canonical field map plus the raw payload kept for audit display.
func parseNotifyBody(r *http.Request, body []byte) (map[string]string, string) {
	if r.Method == http.MethodGet {
		query := r.URL.Query()
		query.Del("secret")

		return firstValues(query), query.Encode()
	}

	fields := map[string]string{}

	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err == nil {
			for k, v := range m {
				switch t := v.(type) {
				case string:
					fields[k] = t
				case float64:
					fields[k] = strconv.FormatInt(int64(t), 10)
				}
			}

			// Accept snake_case JSON keys too.
			alias := map[string]string{
				"call_sid": "CallSid", "account_sid": "AccountSid",
				"answered_by":                "AnsweredBy",
				"machine_detection_duration": "MachineDetectionDuration",
				"from":                       "From", "to": "To", "call_status": "CallStatus",
				"direction": "Direction", "sip_call_id": "SipCallId",
				"fs_uuid": "FsUuid", "call_duration": "CallDuration",
				"sip_response_code": "SipResponseCode",
				"error_code":        "ErrorCode", "error_message": "ErrorMessage",
				"sequence_number": "SequenceNumber", "timestamp": "Timestamp",
				"parent_call_sid": "ParentCallSid",
			}
			for k, canon := range alias {
				if v, ok := fields[k]; ok && fields[canon] == "" {
					fields[canon] = v
				}
			}
		}

		return fields, string(body)
	}

	// Form-encoded body (already read by the handler, so parse from
	// the bytes rather than r.ParseForm).
	form, _ := url.ParseQuery(string(body))
	fields = firstValues(form)

	// Query parameters double as fields for GET-style webhook senders.
	for k, vs := range r.URL.Query() {
		if k == "secret" {
			continue
		}

		if _, ok := fields[k]; !ok && len(vs) > 0 {
			fields[k] = vs[0]
		}
	}

	return fields, string(body)
}

// firstValues keeps each key's first value.
func firstValues(values url.Values) map[string]string {
	fields := make(map[string]string, len(values))

	for k, vs := range values {
		if len(vs) > 0 {
			fields[k] = vs[0]
		}
	}

	return fields
}

// dispositionTimeLayouts are the Timestamp shapes accepted: RFC 3339
// and RFC 2822 (VoiceML and Twilio send RFC 1123Z), including single-
// digit days.
var dispositionTimeLayouts = []string{
	time.RFC3339Nano, time.RFC1123Z, time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
}

// parseDispositionOrder reads the event-order fields. One that does not
// parse is logged and ignored: losing its order is better than losing
// the call's final outcome by rejecting the whole callback.
func parseDispositionOrder(fields map[string]string) (*int64, time.Time) {
	var seq *int64

	if value := fields["SequenceNumber"]; value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
			seq = &n
		} else {
			log.Printf("[notify] ignoring unparseable SequenceNumber %q (CallSid=%q)", value, fields["CallSid"])
		}
	}

	if value := fields["Timestamp"]; value != "" {
		for _, layout := range dispositionTimeLayouts {
			if at, err := time.Parse(layout, value); err == nil {
				return seq, at
			}
		}

		log.Printf("[notify] ignoring unparseable Timestamp %q (CallSid=%q)", value, fields["CallSid"])
	}

	return seq, time.Time{}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}

	return ""
}
