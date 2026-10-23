package recording

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOpenExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "excl.wav")

	w, err := OpenExclusive(path, 8000, 1)
	if err != nil {
		t.Fatalf("OpenExclusive: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := OpenExclusive(path, 8000, 1); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second OpenExclusive err = %v, want os.ErrExist", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(data) != 44 {
		t.Errorf("existing file was modified: size %d, want 44", len(data))
	}
}

func TestOpenInvalidChannels(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.wav"), 8000, 0); err == nil {
		t.Error("expected error for channels 0")
	}
}

func TestOpenMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no", "such", "dir", "x.wav")
	if _, err := Open(path, 8000, 1); err == nil || !strings.Contains(err.Error(), "open wav") {
		t.Fatalf("err = %v, want open wav error", err)
	}
}

func TestOpenReadOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := Open(filepath.Join(dir, "x.wav"), 8000, 1); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want permission error", err)
	}
}

func TestOpenHeaderOutOfRangeRemovesFile(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name       string
		sampleRate int
		channels   int
		want       string
	}{
		{"channels", 8000, 0x10000, "channels out of range"},
		{"sampleRate", 0x80000000, 1, "sampleRate out of range"},
	}

	for _, tc := range cases {
		path := filepath.Join(dir, tc.name+".wav")

		_, err := Open(path, tc.sampleRate, tc.channels)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}

		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: file left behind after failed open (stat err %v)", tc.name, err)
		}
	}
}

func TestOpenHeaderWriteFailure(t *testing.T) {
	// /dev/full accepts open but every write fails with ENOSPC. A symlink
	// keeps the cleanup os.Remove pointed at the link, not the device.
	path := filepath.Join(t.TempDir(), "full.wav")
	if err := os.Symlink("/dev/full", path); err != nil {
		t.Fatal(err)
	}

	_, err := Open(path, 8000, 1)
	if err == nil || !strings.Contains(err.Error(), "wav header") {
		t.Fatalf("err = %v, want wav header error", err)
	}

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink not removed after failed header write (lstat err %v)", err)
	}
}

func TestAppendReusesScratchAndEncodesLittleEndian(t *testing.T) {
	path := filepath.Join(t.TempDir(), "le.wav")

	w, err := Open(path, 8000, 1)
	if err != nil {
		t.Fatal(err)
	}

	if n, err := w.Append([]int16{1, -2, 0x1234}); err != nil || n != 3 {
		t.Fatalf("Append = %d, %v", n, err)
	}

	if n, err := w.Append([]int16{-1}); err != nil || n != 1 {
		t.Fatalf("second Append = %d, %v", n, err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Append([]int16{1}); err == nil {
		t.Error("expected error appending to closed writer")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := []byte{0x01, 0x00, 0xFE, 0xFF, 0x34, 0x12, 0xFF, 0xFF}
	if string(data[44:]) != string(want) {
		t.Errorf("samples = % x, want % x", data[44:], want)
	}

	if got := binary.LittleEndian.Uint32(data[40:44]); got != 8 {
		t.Errorf("data size = %d, want 8", got)
	}

	if got := binary.LittleEndian.Uint32(data[4:8]); got != 44 {
		t.Errorf("riff size = %d, want 44", got)
	}
}

func TestAppendWriteError(t *testing.T) {
	f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	w := &Writer{f: f, sampleRate: 8000, channels: 1}

	if n, err := w.Append([]int16{1, 2}); err == nil || n != 0 {
		t.Fatalf("Append = %d, %v; want 0 and error", n, err)
	}

	if got := w.DurationSec(); got != 0 {
		t.Errorf("DurationSec after failed write = %d, want 0", got)
	}

	// Seek succeeds on /dev/full but the size patch write fails.
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "wav write data size") {
		t.Fatalf("Close err = %v, want data size write error", err)
	}
}

func TestCloseSeekError(t *testing.T) {
	r, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = r.Close() })

	w := &Writer{f: wr, sampleRate: 8000, channels: 1}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "wav seek data size") {
		t.Fatalf("Close err = %v, want seek error", err)
	}

	if err := w.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestDurationSecZeroParams(t *testing.T) {
	if got := (&Writer{samples: 100}).DurationSec(); got != 0 {
		t.Errorf("DurationSec = %d, want 0", got)
	}

	if got := (&Writer{samples: 100, sampleRate: 8000}).DurationSec(); got != 0 {
		t.Errorf("DurationSec with zero channels = %d, want 0", got)
	}
}

func TestCeilSeconds(t *testing.T) {
	cases := []struct {
		amount, perSecond int64
		want              int
	}{
		{0, 8000, 0},
		{-1, 8000, 0},
		{100, 0, 0},
		{100, -5, 0},
		{1, 8000, 1},
		{8000, 8000, 1},
		{8001, 8000, 2},
	}

	for _, tc := range cases {
		if got := ceilSeconds(tc.amount, tc.perSecond); got != tc.want {
			t.Errorf("ceilSeconds(%d, %d) = %d, want %d", tc.amount, tc.perSecond, got, tc.want)
		}
	}
}

func TestWriteHeaderPlaceholderFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stereo.wav")

	w, err := Open(path, 16000, 2)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(data[12:16]) != "fmt " || string(data[36:40]) != "data" {
		t.Errorf("chunk ids = %q %q", data[12:16], data[36:40])
	}

	checks := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"fmt size", binary.LittleEndian.Uint32(data[16:]), 16},
		{"format", uint32(binary.LittleEndian.Uint16(data[20:])), 1},
		{"channels", uint32(binary.LittleEndian.Uint16(data[22:])), 2},
		{"sample rate", binary.LittleEndian.Uint32(data[24:]), 16000},
		{"bytes/sec", binary.LittleEndian.Uint32(data[28:]), 64000},
		{"block align", uint32(binary.LittleEndian.Uint16(data[32:])), 4},
		{"bits", uint32(binary.LittleEndian.Uint16(data[34:])), 16},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// crashHeader is the header Writer leaves behind when the process dies
// before Close: the SRS format with both size fields zero.
func crashHeader() []byte {
	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], 8000)
	binary.LittleEndian.PutUint32(h[28:], 16000)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:40], "data")

	return h
}

func writeCrashedWAV(t *testing.T, dataBytes int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "crashed.wav")
	header := crashHeader()

	if err := os.WriteFile(path, append(header, make([]byte, dataBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestRepairWAV_MissingFile(t *testing.T) {
	_, err := RepairWAV(filepath.Join(t.TempDir(), "missing.wav"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestRepairWAV_ReadOnlyFile(t *testing.T) {
	path := writeCrashedWAV(t, 16)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}

	if _, err := RepairWAV(path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want permission error", err)
	}
}

func TestRepairWAV_DropsTornSample(t *testing.T) {
	path := writeCrashedWAV(t, 16001) // 8000 samples + one torn byte

	dur, err := RepairWAV(path)
	if err != nil {
		t.Fatalf("RepairWAV: %v", err)
	}

	if dur != 1 {
		t.Errorf("duration = %d, want 1", dur)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := binary.LittleEndian.Uint32(data[40:44]); got != 16000 {
		t.Errorf("data size = %d, want 16000", got)
	}

	if got := binary.LittleEndian.Uint32(data[4:8]); got != 36+16000 {
		t.Errorf("riff size = %d, want %d", got, 36+16000)
	}
}

func TestRepairWAV_EmptyDataChunk(t *testing.T) {
	path := writeCrashedWAV(t, 0)
	// A header-only file needs riff=36, data=0; a zeroed header is wrong
	// in the riff field only.
	dur, err := RepairWAV(path)
	if err != nil || dur != 0 {
		t.Fatalf("RepairWAV = %d, %v; want 0, nil", dur, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := binary.LittleEndian.Uint32(data[4:8]); got != 36 {
		t.Errorf("riff size = %d, want 36", got)
	}
}

func TestRepairWAV_RejectsHeaderSizedNonWAV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notwav.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 64)), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := RepairWAV(path)
	if err == nil || !strings.Contains(err.Error(), "not a RIFF/WAVE file") {
		t.Fatalf("err = %v, want not-RIFF error", err)
	}

	riffOnly := writeCrashedWAV(t, 0)

	data, err := os.ReadFile(riffOnly)
	if err != nil {
		t.Fatal(err)
	}

	copy(data[8:12], "AVI ")

	if err := os.WriteFile(riffOnly, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RepairWAV(riffOnly); err == nil || !strings.Contains(err.Error(), "not a RIFF/WAVE file") {
		t.Fatalf("RIFF/AVI err = %v, want not-RIFF error", err)
	}
}

// repairUnderFileSizeLimit runs RepairWAV with RLIMIT_FSIZE lowered to
// limit bytes, so writes at or past that offset fail with EFBIG (the Go
// runtime ignores SIGXFSZ). The limit is restored before returning.
func repairUnderFileSizeLimit(t *testing.T, path string, limit uint64) error {
	t.Helper()

	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}

	lowered := orig
	lowered.Cur = limit

	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lowered); err != nil {
		t.Fatal(err)
	}

	_, err := RepairWAV(path)

	if rerr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig); rerr != nil {
		t.Fatalf("restore RLIMIT_FSIZE: %v", rerr)
	}

	return err
}

func TestRepairWAV_PatchWriteFailures(t *testing.T) {
	cases := []struct {
		limit uint64
		want  string
	}{
		{2, "patch riff size"},  // offset 4 is past the limit
		{10, "patch data size"}, // offset 4 fits, offset 40 does not
	}

	for _, tc := range cases {
		path := writeCrashedWAV(t, 16)

		err := repairUnderFileSizeLimit(t, path, tc.limit)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, syscall.EFBIG) {
			t.Errorf("limit %d: err = %v, want %q wrapping EFBIG", tc.limit, err, tc.want)
		}
	}
}

func TestRepairWAV_RejectsOversizeFile(t *testing.T) {
	path := writeCrashedWAV(t, 0)

	// Sparse extension past the uint32 size space; no real disk use.
	if err := os.Truncate(path, 44+0xFFFFFFFF); err != nil {
		t.Fatal(err)
	}

	_, err := RepairWAV(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds the WAV uint32 size space") {
		t.Fatalf("err = %v, want size-space error", err)
	}
}
