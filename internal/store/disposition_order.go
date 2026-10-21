package store

import "database/sql"

func terminalDisposition(status string) bool {
	switch status {
	case "completed", "busy", "no-answer", "failed", "canceled":
		return true
	}
	return false
}

func staleDisposition(status string, seq sql.NullInt64, at int64, n DispositionNotification) bool {
	if terminalDisposition(status) && !terminalDisposition(n.CallStatus) {
		return true
	}
	if seq.Valid && n.SequenceNumber != nil {
		return *n.SequenceNumber <= seq.Int64
	}
	if at != 0 && !n.Timestamp.IsZero() {
		return millis(n.Timestamp) <= at
	}
	// Without comparable ordering, retain an established terminal outcome.
	return terminalDisposition(status)
}
