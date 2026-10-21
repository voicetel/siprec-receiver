// Package recording captures live call audio to WAV files.
//
// The Writer is fed PCM samples (int16 little-endian at the call's
// sample rate, mono). It writes a
// 44-byte WAV header on Open, streams int16 samples into the file as
// they arrive, and patches the data-size + RIFF-size fields on Close so
// the file is a valid RIFF WAVE container that media players accept.
//
// Concurrency: each Writer is owned by a single session goroutine. Do
// NOT share a Writer across goroutines.
package recording

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Writer streams int16 PCM samples to a WAV file.
type Writer struct {
	f          *os.File
	sampleRate int
	channels   int
	samples    int64 // total int16 samples written
	closed     bool

	// scratch is the per-Writer encoding buffer. Append refills it on
	// every call (overwrite, not append). Sized to grow on demand —
	// at 8 kHz/20 ms frames the steady state is 320 bytes, but
	// wideband (16 kHz) or larger feed-batches grow it once. Owned
	// by the single session goroutine that drives this Writer; not
	// safe for concurrent Append calls (matches the package contract
	// already documented at the package level).
	scratch []byte
}

// Open creates a new WAV file at path (truncating any existing file),
// ready to accept samples.
//
// The file mode is 0o600 — recordings often contain customer audio and
// should not be world-readable. Caller is responsible for choosing a
// path inside an appropriate retention-managed directory.
func Open(path string, sampleRate, channels int) (*Writer, error) {
	return open(path, sampleRate, channels, false)
}

// OpenExclusive is Open with O_EXCL: it fails with an error matching
// os.ErrExist when the file already exists, so a caller generating
// unique names can prove it never overwrote a prior recording.
func OpenExclusive(path string, sampleRate, channels int) (*Writer, error) {
	return open(path, sampleRate, channels, true)
}

func open(path string, sampleRate, channels int, exclusive bool) (*Writer, error) {
	if sampleRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("invalid wav params: sampleRate=%d channels=%d", sampleRate, channels)
	}

	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if exclusive {
		flags = os.O_CREATE | os.O_WRONLY | os.O_EXCL
	}

	f, err := os.OpenFile(path, flags, 0o600) // #nosec G304 -- path is server-generated or operator-configured
	if err != nil {
		return nil, fmt.Errorf("open wav %s: %w", path, err)
	}

	w := &Writer{
		f:          f,
		sampleRate: sampleRate,
		channels:   channels,
	}

	if err := w.writeHeaderPlaceholder(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)

		return nil, err
	}

	return w, nil
}

// Append streams a chunk of samples to the file. Returns the number of
// samples actually written (== len(pcm) on success).
func (w *Writer) Append(pcm []int16) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("wav: write to closed writer")
	}

	// Reuse the per-Writer scratch buffer — Append used to allocate
	// 2*len(pcm) bytes per call (~50/sec/active recording at 20 ms
	// frames). The contract: scratch's contents are written to disk
	// before Append returns and never retained, so overwriting on
	// the next call is safe.
	need := 2 * len(pcm)
	if cap(w.scratch) < need {
		w.scratch = make([]byte, need)
	} else {
		w.scratch = w.scratch[:need]
	}

	// Stream-write little-endian int16. binary.Write would re-flect each
	// element; we do it inline because this is in the per-frame hot path
	// (called every ~10–20 ms while a recording is active).
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(w.scratch[2*i:], uint16(s)) // #nosec G115 -- int16 sample bit pattern
	}

	if _, err := w.f.Write(w.scratch); err != nil {

		return 0, fmt.Errorf("wav write: %w", err)
	}

	w.samples += int64(len(pcm))

	return len(pcm), nil
}

// DurationSec returns the recorded duration in seconds, rounded up as
// Twilio reports a recording's duration (ceilSeconds).
func (w *Writer) DurationSec() int {
	if w.sampleRate == 0 || w.channels == 0 {
		return 0
	}

	return ceilSeconds(w.samples, int64(w.sampleRate*w.channels))
}

// Close finalises the WAV header (data + RIFF chunk sizes) and closes
// the file. Safe to call multiple times.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}

	w.closed = true

	dataBytes := w.samples * 2 // int16 = 2 bytes per sample

	// data chunk size at offset 40, RIFF size at offset 4.
	if _, err := w.f.Seek(40, io.SeekStart); err != nil {
		_ = w.f.Close()

		return fmt.Errorf("wav seek data size: %w", err)
	}

	if err := binary.Write(w.f, binary.LittleEndian, uint32(dataBytes)); err != nil { // #nosec G115 -- WAV size fields are uint32 by spec; the SRS leg sample cap bounds file size
		_ = w.f.Close()

		return fmt.Errorf("wav write data size: %w", err)
	}

	if _, err := w.f.Seek(4, io.SeekStart); err != nil {
		_ = w.f.Close()

		return fmt.Errorf("wav seek riff size: %w", err)
	}

	if err := binary.Write(w.f, binary.LittleEndian, uint32(36+dataBytes)); err != nil { // #nosec G115 -- WAV size fields are uint32 by spec; the SRS leg sample cap bounds file size
		_ = w.f.Close()

		return fmt.Errorf("wav write riff size: %w", err)
	}

	return w.f.Close()
}

// ceilSeconds is a media length in whole seconds, rounded up (a 1.8 s
// recording reports 2). Zero media is 0.
func ceilSeconds(amount, perSecond int64) int {
	if perSecond <= 0 || amount <= 0 {
		return 0
	}

	return int((amount + perSecond - 1) / perSecond)
}

// writeHeaderPlaceholder writes a 44-byte WAV header with size fields
// zeroed out. Close patches them in.
//
// Open already validates that sampleRate and channels are positive, so
// each value comfortably fits its WAV-spec field (uint32 / uint16) and
// the conversions below cannot truncate. Bounds are checked explicitly
// here as belt-and-braces against future API drift.
func (w *Writer) writeHeaderPlaceholder() error {
	if w.channels <= 0 || w.channels > 0xFFFF {
		return fmt.Errorf("wav: channels out of range: %d", w.channels)
	}

	if w.sampleRate <= 0 || w.sampleRate > 0x7FFFFFFF {
		return fmt.Errorf("wav: sampleRate out of range: %d", w.sampleRate)
	}

	channels16 := uint16(w.channels)
	sampleRate32 := uint32(w.sampleRate)
	bytesPerSec := sampleRate32 * uint32(channels16) * 2
	blockAlign := channels16 * 2
	const bitsPerSample uint16 = 16

	buf := make([]byte, 44)
	copy(buf[0:4], "RIFF")
	// 4..8 RIFF size — patched on Close
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(buf[20:], 1)  // PCM format
	binary.LittleEndian.PutUint16(buf[22:], channels16)
	binary.LittleEndian.PutUint32(buf[24:], sampleRate32)
	binary.LittleEndian.PutUint32(buf[28:], bytesPerSec)
	binary.LittleEndian.PutUint16(buf[32:], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:], bitsPerSample)
	copy(buf[36:40], "data")
	// 40..44 data chunk size — patched on Close

	if _, err := w.f.Write(buf); err != nil {
		return fmt.Errorf("wav header: %w", err)
	}

	return nil
}
