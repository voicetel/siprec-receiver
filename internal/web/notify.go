package web

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/voicetel/siprec-receiver/internal/store"
)

// handleNotifyAMD is the call-notification endpoint: it logs the AMD
// (answering-machine detection) disposition for a call.
//
// It accepts the exact form field names VoiceML POSTs to an
// AsyncAmdStatusCallback URL (CallSid, AccountSid, AnsweredBy,
// MachineDetectionDuration, From, To, CallStatus, Direction, Caller,
// Called, ApiVersion) as form-encoding or JSON, plus the SIPREC
// correlation keys a poster that knows them can add:
//
//	SipCallId — the SIPREC recording dialog's SIP Call-ID
//	FsUuid    — the FreeSWITCH parent call UUID (the RS-Metadata
//	            session_id, base64-decoded, identifies the same call)
//	CallSid   — matched against a recording whose INVITE Request-URI
//	            carried ;vm-callsid=CA…
//
// Auth is a shared secret (NOTIFY_SECRET): Authorization: Bearer,
// X-Notify-Secret, or ?secret= (the query form exists for webhook
// senders like VoiceML's that cannot set custom headers).
// When NOTIFY_SECRET is unset the endpoint is disabled (503) rather
// than open.
func (s *Server) handleNotifyAMD(w http.ResponseWriter, r *http.Request) {
	if s.NotifySecret == "" {
		http.Error(w, "notify endpoint disabled (NOTIFY_SECRET not set)", http.StatusServiceUnavailable)

		return
	}

	if !s.notifyAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)

		return
	}

	fields, raw := parseNotifyBody(r, body)

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
		Raw:        raw,
	}

	id, matched, err := s.Store.ApplyAMD(r.Context(), n)
	if errors.Is(err, store.ErrNoKey) {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if err != nil {
		log.Printf("[notify] apply amd (CallSid=%q SipCallId=%q): %v", n.CallSid, n.SipCallID, err)
		http.Error(w, "failed to log disposition", http.StatusInternalServerError)

		return
	}

	log.Printf("[notify] amd logged id=%d matched=%v answeredBy=%q CallSid=%q",
		id, matched, n.AnsweredBy, n.CallSid)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "matched": matched})
}

// handleNotifyDisposition is the final-outcome companion to
// handleNotifyAMD: POST /api/notify/disposition logs what ultimately
// happened to the call (the StatusCallback "completed" event's
// content). Same auth, body shapes, and correlation keys as the AMD
// endpoint; CallStatus is required instead of AnsweredBy. A
// disposition carrying AnsweredBy also fills still-empty AMD fields.
func (s *Server) handleNotifyDisposition(w http.ResponseWriter, r *http.Request) {
	if s.NotifySecret == "" {
		http.Error(w, "notify endpoint disabled (NOTIFY_SECRET not set)", http.StatusServiceUnavailable)

		return
	}

	if !s.notifyAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)

		return
	}

	fields, raw := parseNotifyBody(r, body)

	if fields["CallStatus"] == "" {
		http.Error(w, "CallStatus is required", http.StatusBadRequest)

		return
	}

	durationSec, _ := strconv.Atoi(firstNonEmpty(fields["CallDuration"], fields["Duration"]))
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
		Raw:            raw,
	}

	id, matched, err := s.Store.ApplyDisposition(r.Context(), n)
	if errors.Is(err, store.ErrNoKey) {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if err != nil {
		log.Printf("[notify] apply disposition (CallSid=%q SipCallId=%q): %v", n.CallSid, n.SipCallID, err)
		http.Error(w, "failed to log disposition", http.StatusInternalServerError)

		return
	}

	log.Printf("[notify] disposition logged id=%d matched=%v status=%q CallSid=%q",
		id, matched, n.CallStatus, n.CallSid)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "matched": matched})
}

func (s *Server) notifyAuthorized(r *http.Request) bool {
	querySecret := r.URL.Query().Get("secret")
	if querySecret != "" && hmac.Equal([]byte(querySecret), []byte(s.NotifySecret)) {
		// The query form exists for webhook senders that cannot set
		// headers, but URLs land in proxy/access logs — say so once.
		s.secretQueryWarn.Do(func() {
			log.Printf("[notify] WARNING: notify secret presented as ?secret= query parameter; " +
				"it will appear in request logs. Prefer the Authorization: Bearer header.")
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

// parseNotifyBody accepts JSON or form-encoding (VoiceML/Twilio
// webhooks are form-encoded) and returns a canonical field map plus
// the raw body kept for audit display.
func parseNotifyBody(r *http.Request, body []byte) (map[string]string, string) {
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
				"duration": "Duration", "sip_response_code": "SipResponseCode",
				"error_code": "ErrorCode", "error_message": "ErrorMessage",
				"sequence_number": "SequenceNumber", "timestamp": "Timestamp",
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
	for k, vs := range form {
		if len(vs) > 0 {
			fields[k] = vs[0]
		}
	}

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
