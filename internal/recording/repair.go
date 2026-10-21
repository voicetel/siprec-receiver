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
// The sample rate is assumed to be 8000 Hz mono (the only format the
// SRS writes); duration follows the Writer's round-up rule.
func RepairWAV(path string) (durationSec int, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
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

	dataBytes := info.Size() - 44
	if dataBytes%2 != 0 {
		dataBytes-- // a torn final sample is not playable; drop it
	}

	riffSize := uint32(36 + dataBytes)
	dataSize := uint32(dataBytes)

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
