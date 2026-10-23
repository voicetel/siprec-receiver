package recording

import (
	"encoding/binary"
	"fmt"
	"os"
)

// RepairWAV makes a WAV file left behind by a crashed or killed
// process playable again and reports its duration.
//
// Our Writer patches the RIFF/data size fields only on Close. A
// process that dies mid-recording leaves a file whose header still
// carries the placeholder (zero) sizes while the data chunk holds
// every sample written before the crash. RepairWAV recomputes the
// sizes from the actual file length (the writer emits no chunks
// beyond fmt/data, so data is everything after the 44-byte header)
// and rewrites the two size fields in place. Files that are already
// valid are left untouched.
//
// Only the exact header the Writer emits is repaired: fmt at 12 (16
// bytes of 8000 Hz mono 16-bit PCM) and data at 36. Patching any other
// WAV (one with a LIST chunk, another format, a file someone dropped
// into the directory) at those offsets would corrupt it, so it is
// refused and left alone. Duration follows the Writer's round-up rule.
func RepairWAV(path string) (durationSec int, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- path from the recording directory or a leg row
	if err != nil {
		return 0, fmt.Errorf("repair wav %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("repair wav %s: stat: %w", path, err)
	}

	if info.Size() < 44 {
		return 0, fmt.Errorf("repair wav %s: %d bytes, shorter than a WAV header", path, info.Size())
	}

	header := make([]byte, 44)
	if _, err := f.ReadAt(header, 0); err != nil {
		return 0, fmt.Errorf("repair wav %s: read header: %w", path, err)
	}

	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, fmt.Errorf("repair wav %s: not a RIFF/WAVE file", path)
	}

	if !writerHeader(header) {
		return 0, fmt.Errorf("repair wav %s: not a header this recorder writes (8000 Hz mono 16-bit PCM, data at 36)", path)
	}

	dataBytes := info.Size() - 44
	if dataBytes < 0 || dataBytes > 0xFFFFFFFF-36 {
		return 0, fmt.Errorf("repair wav %s: %d data bytes exceeds the WAV uint32 size space", path, dataBytes)
	}

	if dataBytes%2 != 0 {
		dataBytes-- // a torn final sample is not playable; drop it
	}

	riffSize := uint32(36 + dataBytes) // #nosec G115 -- range-checked above
	dataSize := uint32(dataBytes)      // #nosec G115 -- range-checked above

	if binary.LittleEndian.Uint32(header[4:8]) != riffSize ||
		binary.LittleEndian.Uint32(header[40:44]) != dataSize {
		var buf [4]byte

		binary.LittleEndian.PutUint32(buf[:], riffSize)
		if _, err := f.WriteAt(buf[:], 4); err != nil {
			return 0, fmt.Errorf("repair wav %s: patch riff size: %w", path, err)
		}

		binary.LittleEndian.PutUint32(buf[:], dataSize)
		if _, err := f.WriteAt(buf[:], 40); err != nil {
			return 0, fmt.Errorf("repair wav %s: patch data size: %w", path, err)
		}

		if err := f.Sync(); err != nil {
			return 0, fmt.Errorf("repair wav %s: sync: %w", path, err)
		}
	}

	samples := dataBytes / 2

	return ceilSeconds(samples, 8000), nil
}

// writerHeader reports whether a 44-byte header is the one Writer
// emits for the SRS's format, size fields aside.
func writerHeader(h []byte) bool {
	le := binary.LittleEndian

	return string(h[12:16]) == "fmt " && le.Uint32(h[16:20]) == 16 &&
		le.Uint16(h[20:22]) == 1 && // PCM
		le.Uint16(h[22:24]) == 1 && // mono
		le.Uint32(h[24:28]) == 8000 &&
		le.Uint32(h[28:32]) == 16000 && le.Uint16(h[32:34]) == 2 && // byte rate, block align
		le.Uint16(h[34:36]) == 16 &&
		string(h[36:40]) == "data"
}
