package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Row resolution. A recording leg and a notification each carry some of
// three correlation keys, and earlier events may each have created a row
// under a different one: an AMD webhook keyed by CallSid, a disposition
// keyed by FsUUID, the recording keyed by its SIP dialog. resolveCall
// finds every row the keys name and folds them into one, so a call never
// stays split across rows (and a key never collides with a UNIQUE index
// because another row already holds it).

// callKeys are the correlation keys one event carries; empty means
// absent.
type callKeys struct {
	CallSid   string
	SipCallID string // a SIPREC dialog's Call-ID: the row's own or any leg's
	FsUUID    string
}

// candidateRow is one row a key names.
type candidateRow struct {
	id        int64
	callSid   string
	completed bool
	rank      int // which key found it: 0 CallSid, 1 dialog, 2 FsUUID
}

const candidateCols = `SELECT id, COALESCE(call_sid, ''), status FROM calls`

// findCandidates returns every row the keys name, each once, in key
// order (CallSid, dialog, FsUUID).
func findCandidates(ctx context.Context, tx *sql.Tx, k callKeys) ([]candidateRow, error) {
	var out []candidateRow

	seen := map[int64]bool{}

	add := func(rank int, query string, args ...any) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("store: find call rows: %w", err)
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				c      candidateRow
				status string
			)

			if err := rows.Scan(&c.id, &c.callSid, &status); err != nil {
				return fmt.Errorf("store: scan call row: %w", err)
			}

			if seen[c.id] {
				continue
			}

			seen[c.id] = true
			c.completed, c.rank = status == "completed", rank
			out = append(out, c)
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: find call rows: %w", err)
		}

		return nil
	}

	if k.CallSid != "" {
		if err := add(0, candidateCols+` WHERE call_sid = ?`, k.CallSid); err != nil {
			return nil, err
		}
	}

	if k.SipCallID != "" {
		if err := add(1, candidateCols+` WHERE sip_call_id = ?
			OR id IN (SELECT call_id FROM legs WHERE sip_call_id = ?) ORDER BY id`,
			k.SipCallID, k.SipCallID); err != nil {
			return nil, err
		}
	}

	if k.FsUUID != "" {
		if err := add(2, candidateCols+` WHERE fs_uuid = ? ORDER BY id`, k.FsUUID); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// resolveCall returns the one row the keys name, after merging every
// other matching row into it, or 0 when no row matches. A row whose
// CallSid differs from the event's (or, for an event with none, from the
// first CallSid found) belongs to another call and is left alone. The
// survivor is the oldest row holding a recording, else the oldest row
// found by the strongest key; it gains the event's CallSid.
func resolveCall(ctx context.Context, tx *sql.Tx, k callKeys) (int64, error) {
	cands, err := findCandidates(ctx, tx, k)
	if err != nil || len(cands) == 0 {
		return 0, err
	}

	callSid := k.CallSid
	for _, c := range cands {
		if callSid != "" {
			break
		}

		callSid = c.callSid
	}

	keep := make([]candidateRow, 0, len(cands))

	for _, c := range cands {
		if c.callSid == "" || c.callSid == callSid {
			keep = append(keep, c)
		}
	}

	if len(keep) == 0 {
		return 0, nil
	}

	sort.SliceStable(keep, func(i, j int) bool {
		if keep[i].completed != keep[j].completed {
			return keep[i].completed
		}

		if keep[i].rank != keep[j].rank {
			return keep[i].rank < keep[j].rank
		}

		return keep[i].id < keep[j].id
	})

	survivor := keep[0]

	for _, v := range keep[1:] {
		if err := mergeCalls(ctx, tx, survivor.id, v.id); err != nil {
			return 0, err
		}
	}

	// Every row holding callSid was a candidate and is now merged, so
	// the survivor can take it without tripping the UNIQUE index.
	if callSid != "" && survivor.callSid == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE calls SET call_sid = ? WHERE id = ?`, callSid, survivor.id); err != nil {
			return 0, fmt.Errorf("store: adopt call_sid: %w", err)
		}
	}

	return survivor.id, nil
}

// mergeRow is the subset of a call row mergeCalls folds.
type mergeRow struct {
	sipCallID, callSid, fsUUID, accountSid, fromURI string
	fromNumber, toNumber, direction, status         string
	startedAt, endedAt                              int64
	amdBy, amdStatus, amdRaw                        string
	amdDurMs                                        int
	amdAt                                           int64
	metaJSON, metaXML                               string
	dispStatus, dispErrCode, dispErrMsg, dispRaw    string
	dispDur, dispSip                                int
	dispAt, dispEventAt                             int64
	dispSeq                                         sql.NullInt64
	vmJSON                                          string
	vmAt                                            int64
	parentCallSid                                   string
}

func loadMergeRow(ctx context.Context, tx *sql.Tx, id int64) (mergeRow, error) {
	var r mergeRow

	err := tx.QueryRowContext(ctx, `SELECT COALESCE(sip_call_id, ''), COALESCE(call_sid, ''),
		COALESCE(fs_uuid, ''), account_sid, from_uri, from_number, to_number, direction, status,
		started_at, ended_at, amd_answered_by, amd_call_status, amd_raw, amd_duration_ms,
		amd_notified_at, metadata_json, metadata_xml, disp_status, disp_error_code,
		disp_error_message, disp_raw, disp_duration_sec, disp_sip_code, disp_notified_at,
		disp_event_at, disp_sequence, voiceml_record_json, voiceml_fetched_at, parent_call_sid
		FROM calls WHERE id = ?`, id).Scan(&r.sipCallID, &r.callSid, &r.fsUUID, &r.accountSid,
		&r.fromURI, &r.fromNumber, &r.toNumber, &r.direction, &r.status, &r.startedAt,
		&r.endedAt, &r.amdBy, &r.amdStatus, &r.amdRaw, &r.amdDurMs, &r.amdAt, &r.metaJSON,
		&r.metaXML, &r.dispStatus, &r.dispErrCode, &r.dispErrMsg, &r.dispRaw, &r.dispDur,
		&r.dispSip, &r.dispAt, &r.dispEventAt, &r.dispSeq, &r.vmJSON, &r.vmAt, &r.parentCallSid)
	if err != nil {
		return mergeRow{}, fmt.Errorf("store: load call %d: %w", id, err)
	}

	return r, nil
}

// fill returns keep unless it is empty.
func fill(keep, other string) string {
	if keep != "" {
		return keep
	}

	return other
}

// mergeCalls folds row dropID into row keepID and deletes it: legs move
// over, identity and metadata fill what keepID lacks, the AMD verdict
// and the fetched VoiceML record fill when keepID has none, and the
// disposition follows the same ordering rules as a new notification
// (a later or terminal outcome wins).
func mergeCalls(ctx context.Context, tx *sql.Tx, keepID, dropID int64) error {
	keep, err := loadMergeRow(ctx, tx, keepID)
	if err != nil {
		return err
	}

	drop, err := loadMergeRow(ctx, tx, dropID)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE legs SET call_id = ? WHERE call_id = ?`, keepID, dropID); err != nil {
		return fmt.Errorf("store: move legs %d -> %d: %w", dropID, keepID, err)
	}

	// Delete first: the dropped row's UNIQUE keys move to the survivor.
	if _, err := tx.ExecContext(ctx, `DELETE FROM calls WHERE id = ?`, dropID); err != nil {
		return fmt.Errorf("store: delete merged call %d: %w", dropID, err)
	}

	m := keep
	m.sipCallID, m.callSid = fill(keep.sipCallID, drop.sipCallID), fill(keep.callSid, drop.callSid)
	m.fsUUID, m.accountSid = fill(keep.fsUUID, drop.fsUUID), fill(keep.accountSid, drop.accountSid)
	m.fromURI, m.fromNumber = fill(keep.fromURI, drop.fromURI), fill(keep.fromNumber, drop.fromNumber)
	m.toNumber, m.direction = fill(keep.toNumber, drop.toNumber), fill(keep.direction, drop.direction)
	m.metaJSON, m.metaXML = fill(keep.metaJSON, drop.metaJSON), fill(keep.metaXML, drop.metaXML)
	m.parentCallSid = fill(keep.parentCallSid, drop.parentCallSid)
	m.startedAt = mergedStart(keep, drop)
	m.endedAt = max(keep.endedAt, drop.endedAt)

	if drop.status == "completed" {
		m.status = "completed"
	}

	if keep.amdBy == "" && drop.amdBy != "" {
		m.amdBy, m.amdStatus, m.amdRaw = drop.amdBy, drop.amdStatus, drop.amdRaw
		m.amdDurMs, m.amdAt = drop.amdDurMs, drop.amdAt
	}

	if takeDisposition(keep, drop) {
		m.dispStatus, m.dispErrCode, m.dispErrMsg, m.dispRaw = drop.dispStatus, drop.dispErrCode, drop.dispErrMsg, drop.dispRaw
		m.dispDur, m.dispSip, m.dispAt, m.dispEventAt, m.dispSeq = drop.dispDur, drop.dispSip, drop.dispAt, drop.dispEventAt, drop.dispSeq
	}

	if keep.vmJSON == "" {
		m.vmJSON, m.vmAt = drop.vmJSON, drop.vmAt
	}

	_, err = tx.ExecContext(ctx, `UPDATE calls SET
		sip_call_id = ?, call_sid = ?, fs_uuid = ?, account_sid = ?, from_uri = ?,
		from_number = ?, to_number = ?, direction = ?, status = ?, started_at = ?, ended_at = ?,
		amd_answered_by = ?, amd_call_status = ?, amd_raw = ?, amd_duration_ms = ?, amd_notified_at = ?,
		metadata_json = ?, metadata_xml = ?, disp_status = ?, disp_error_code = ?,
		disp_error_message = ?, disp_raw = ?, disp_duration_sec = ?, disp_sip_code = ?,
		disp_notified_at = ?, disp_event_at = ?, disp_sequence = ?,
		voiceml_record_json = ?, voiceml_fetched_at = ?, parent_call_sid = ?, updated_at = ?
		WHERE id = ?`,
		nullIfEmpty(m.sipCallID), nullIfEmpty(m.callSid), m.fsUUID, m.accountSid, m.fromURI,
		m.fromNumber, m.toNumber, m.direction, m.status, m.startedAt, m.endedAt,
		m.amdBy, m.amdStatus, m.amdRaw, m.amdDurMs, m.amdAt,
		m.metaJSON, m.metaXML, m.dispStatus, m.dispErrCode,
		m.dispErrMsg, m.dispRaw, m.dispDur, m.dispSip,
		m.dispAt, m.dispEventAt, m.dispSeq,
		m.vmJSON, m.vmAt, m.parentCallSid, millis(time.Now().UTC()), keepID)
	if err != nil {
		return fmt.Errorf("store: merge call %d into %d: %w", dropID, keepID, err)
	}

	return refreshDuration(ctx, tx, keepID)
}

// mergedStart is the merged call's start: a recording's start wins over
// a placeholder's (which is only when its first webhook arrived), and
// otherwise the earliest non-zero time.
func mergedStart(keep, drop mergeRow) int64 {
	keepRec, dropRec := keep.status == "completed", drop.status == "completed"

	switch {
	case dropRec && !keepRec:
		return drop.startedAt
	case keepRec && !dropRec:
		return keep.startedAt
	case keep.startedAt == 0 || (drop.startedAt != 0 && drop.startedAt < keep.startedAt):
		return drop.startedAt
	default:
		return keep.startedAt
	}
}

// takeDisposition reports whether the dropped row's disposition should
// replace the survivor's: it has one, and the survivor's is absent or
// would not count it stale.
func takeDisposition(keep, drop mergeRow) bool {
	if drop.dispStatus == "" {
		return false
	}

	// A survivor status that only a fetched record filled (no callback
	// set disp_notified_at) gives way to a callback's outcome.
	if keep.dispStatus == "" || (keep.dispAt == 0 && drop.dispAt != 0) {
		return true
	}

	n := DispositionNotification{CallStatus: drop.dispStatus, Timestamp: fromMillis(drop.dispEventAt)}
	if drop.dispSeq.Valid {
		n.SequenceNumber = &drop.dispSeq.Int64
	}

	return !staleDisposition(keep.dispStatus, keep.dispSeq, keep.dispEventAt, n)
}

// refreshDuration sets a call's recorded duration from its legs: the
// longest leg of each SIPREC dialog (separate-stream legs run side by
// side), summed over dialogs (a restarted recording adds its time). A
// call without legs keeps its value.
func refreshDuration(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE calls SET duration_sec = COALESCE(
		(SELECT SUM(d) FROM (SELECT MAX(duration_sec) AS d FROM legs WHERE call_id = ? GROUP BY sip_call_id)),
		duration_sec) WHERE id = ?`, id, id)
	if err != nil {
		return fmt.Errorf("store: call %d duration: %w", id, err)
	}

	return nil
}
