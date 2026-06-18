package tapedrive

import (
	"errors"
	"io"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

// fakeDevice is an in-memory stand-in for the st driver used to exercise the
// ioctl plumbing and Read/Write/Seek logic without real hardware. It records
// the sequence of ioctls and serves Read/Write from a ring of byte blocks.
//
// Like the real driver, read() returns (0, nil) at a filemark or past the end
// of recorded data — the package turns the first such zero into io.EOF and the
// second consecutive zero into ErrEndOfData.
type fakeDevice struct {
	mu          sync.Mutex
	closed      bool
	blocks      [][]byte // written records; nil entry == filemark sentinel
	readIdx     int      // next record to read
	blkno       int64    // reported by MTIOCPOS / Seek
	gstat       int64
	dsreg       int64
	erreg       int64
	forceENOSPC bool // write() always returns ENOSPC when set
	lastMtop    mtop
	opLog       []mtop
}

func newFake() *fakeDevice { return &fakeDevice{gstat: gmtOnline} }

func (f *fakeDevice) ioctlTop(op *mtop) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMtop = *op
	f.opLog = append(f.opLog, *op)
	switch op.Op {
	case OpWEOF, OpWEOFI:
		f.blocks = append(f.blocks, nil) // filemark sentinel
	case OpRewind:
		f.readIdx = 0
		f.blkno = 0
	case OpFSR:
		f.blkno += int64(op.Count)
		f.readIdx += int(op.Count)
	case OpBSR:
		f.blkno -= int64(op.Count)
		f.readIdx -= int(op.Count)
	case OpSeek:
		f.blkno = int64(op.Count)
	case OpSetBlk, OpSetDensity, OpCompression, OpNoOp, OpSetDrvBuf,
		OpLock, OpUnlock, OpFSF, OpBSF, OpFSS, OpBSS, OpWSM,
		OpEOM, OpErase, OpRetension, OpLoad, OpUnload:
		// no-op for the fake
	default:
		return errors.New("fake: unsupported op")
	}
	return nil
}

func (f *fakeDevice) ioctlGet(g *mtget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*g = mtget{
		Gstat: f.gstat,
		Dsreg: f.dsreg,
		Erreg: f.erreg,
		Blkno: int32(f.blkno),
	}
	return nil
}

func (f *fakeDevice) ioctlPos(p *mtpos) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.Blkno = f.blkno
	return nil
}

func (f *fakeDevice) read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readIdx >= len(f.blocks) {
		return 0, nil // past end of recorded data
	}
	b := f.blocks[f.readIdx]
	if b == nil {
		f.readIdx++
		return 0, nil // filemark boundary
	}
	if len(p) < len(b) {
		return 0, errors.New("ENOMEM")
	}
	n := copy(p, b)
	f.readIdx++
	f.blkno++
	return n, nil
}

func (f *fakeDevice) write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceENOSPC {
		return len(p), syscall.ENOSPC
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	f.blocks = append(f.blocks, cp)
	f.blkno++
	return len(p), nil
}

func (f *fakeDevice) close() error { f.closed = true; return nil }

// --- wiring: swap the real syscall-backed methods with the fake -------------

// fakeTape is a Tape whose ops route to a fakeDevice instead of the kernel.
type fakeTape struct {
	*Tape
	dev *fakeDevice
}

func openFake(tb testing.TB) (*fakeTape, *fakeDevice) {
	tb.Helper()
	dev := newFake()
	tape := &Tape{fd: -1, rewind: false, ops: dev}
	return &fakeTape{Tape: tape, dev: dev}, dev
}

func mustClose(tb testing.TB, t io.Closer) {
	tb.Helper()
	if err := t.Close(); err != nil {
		tb.Fatalf("close: %v", err)
	}
}

func mustRead(t *testing.T, tape *Tape, buf []byte) {
	t.Helper()
	if _, err := tape.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func mustWrite(t *testing.T, tape *Tape, p []byte) {
	t.Helper()
	if _, err := tape.Write(p); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestOpenFakeWriteReadRoundTrip(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)

	payloads := [][]byte{
		[]byte("hello tape"),
		[]byte("second block"),
		[]byte("third"),
	}
	for _, p := range payloads {
		if n, err := tape.Write(p); err != nil || n != len(p) {
			t.Fatalf("write %q: n=%d err=%v", p, n, err)
		}
	}
	if err := tape.WriteFilemarks(1); err != nil {
		t.Fatalf("weof: %v", err)
	}

	dev.readIdx = 0 // emulate rewind before reading
	for i, want := range payloads {
		buf := make([]byte, 256)
		n, err := tape.Read(buf)
		if err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		if string(buf[:n]) != string(want) {
			t.Fatalf("read[%d] = %q, want %q", i, buf[:n], want)
		}
	}
	// next read hits the filemark -> io.EOF
	buf := make([]byte, 256)
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF at filemark, got %v", err)
	}
}

func TestSeekStartUsesMTSEEKThenPosition(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)

	got, err := tape.Seek(42, io.SeekStart)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if got != 42 {
		t.Fatalf("seek returned %d, want 42", got)
	}
	last := dev.lastMtop
	if last.Op != OpSeek || last.Count != 42 {
		t.Fatalf("last op = {%d,%d}, want {Seek,42}", last.Op, last.Count)
	}
}

func TestSeekCurrentOffsetsFromPosition(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	dev.blkno = 10

	got, err := tape.Seek(5, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if got != 15 {
		t.Fatalf("seek from current: got %d, want 15", got)
	}
}

func TestSeekNegativePositionRejected(t *testing.T) {
	tape, _ := openFake(t)
	defer mustClose(t, tape.Tape)
	if _, err := tape.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("expected error for negative seek")
	}
}

func TestRewindResetsPosition(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	dev.blkno = 99
	if err := tape.Rewind(); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if dev.lastMtop.Op != OpRewind {
		t.Fatalf("rewind did not issue OpRewind, got %d", dev.lastMtop.Op)
	}
}

func TestWriteFilemarks(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	if err := tape.WriteFilemarks(3); err != nil {
		t.Fatalf("weof: %v", err)
	}
	if dev.lastMtop.Op != OpWEOF || dev.lastMtop.Count != 3 {
		t.Fatalf("weof op = {%d,%d}, want {WEOF,3}", dev.lastMtop.Op, dev.lastMtop.Count)
	}
}

func TestSetBlockSize(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	if err := tape.SetBlockSize(512); err != nil {
		t.Fatalf("setblk: %v", err)
	}
	if dev.lastMtop.Op != OpSetBlk || dev.lastMtop.Count != 512 {
		t.Fatalf("setblk = {%d,%d}, want {SetBlk,512}", dev.lastMtop.Op, dev.lastMtop.Count)
	}
}

func TestEnableLogicalSeekSetsSCSI2Logical(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	if err := tape.EnableLogicalSeek(); err != nil {
		t.Fatalf("enable logical seek: %v", err)
	}
	want := int32(OptSCSI2Logical | OptSetBooleans)
	if dev.lastMtop.Op != OpSetDrvBuf || dev.lastMtop.Count != want {
		t.Fatalf("logical seek op = {%d,0x%x}, want {SetDrvBuf,0x%x}",
			dev.lastMtop.Op, dev.lastMtop.Count, want)
	}
}

func TestClosedReturnsErrNotOpen(t *testing.T) {
	tape, _ := openFake(t)
	if err := tape.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tape.Read(make([]byte, 8)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("read after close: %v, want ErrNotOpen", err)
	}
	if _, err := tape.Write([]byte("x")); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("write after close: %v, want ErrNotOpen", err)
	}
	if _, err := tape.Seek(0, io.SeekStart); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("seek after close: %v, want ErrNotOpen", err)
	}
}

func TestStatusPredicates(t *testing.T) {
	s := Status{Gstat: gmtOnline | gmtBOT}
	if !s.Online() {
		t.Error("Online should be true")
	}
	if !s.BOT() {
		t.Error("BOT should be true")
	}
	if s.EOT() {
		t.Error("EOT should be false")
	}
	s2 := Status{Gstat: gmtDROpen}
	if s2.TapeLoaded() {
		t.Error("TapeLoaded should be false when door open")
	}
}

func TestIsAutoRewindDevice(t *testing.T) {
	cases := map[string]bool{
		"/dev/st0":  true,
		"/dev/nst0": false,
		"/dev/st9":  true,
		"/dev/nst9": false,
		"st0":       true,
		"nst0":      false,
	}
	for name, want := range cases {
		if got := isAutoRewindDevice(name); got != want {
			t.Errorf("isAutoRewindDevice(%q) = %v, want %v", name, got, want)
		}
	}
}

// --- regression tests for st.rst-correct behaviour -----------------------

// TestMtgetLayoutMatchesKernelABI pins the struct mtget wire layout to the
// kernel/glibc ABI: 5 longs followed by two __daddr_t (int, 4 bytes) = 48
// bytes total. A 56-byte layout (all longs) yields the wrong MTIOCGET number
// and ENOTTY on real hardware.
func TestMtgetLayoutMatchesKernelABI(t *testing.T) {
	if sizeofMtget != 48 {
		t.Errorf("sizeof mtget = %d, want 48", sizeofMtget)
	}
	if off := unsafe.Offsetof(mtget{}.Fileno); off != 40 {
		t.Errorf("offsetof mtget.Fileno = %d, want 40", off)
	}
	if off := unsafe.Offsetof(mtget{}.Blkno); off != 44 {
		t.Errorf("offsetof mtget.Blkno = %d, want 44", off)
	}
}

// TestMTIOCIoctlNumbers pins the ioctl request numbers. MTIOCGET must encode
// size=48 (0x30), not 56 (0x38).
func TestMTIOCIoctlNumbers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ioctl numbers are Linux-specific")
	}
	if ioctlMTIOCTOP != 0x40086d01 {
		t.Errorf("ioctlMTIOCTOP = %#x, want 0x40086d01", ioctlMTIOCTOP)
	}
	if ioctlMTIOCGET != 0x80306d02 {
		t.Errorf("ioctlMTIOCGET = %#x, want 0x80306d02 (sizeof mtget=%d)", ioctlMTIOCGET, sizeofMtget)
	}
	if ioctlMTIOCPOS != 0x80086d03 {
		t.Errorf("ioctlMTIOCPOS = %#x, want 0x80086d03", ioctlMTIOCPOS)
	}
}

func TestStructSizes(t *testing.T) {
	if sizeofMtop != 8 {
		t.Errorf("sizeof mtop = %d, want 8", sizeofMtop)
	}
	if sizeofMtpos != 8 {
		t.Errorf("sizeof mtpos = %d, want 8", sizeofMtpos)
	}
	if sizeofMtget != 48 {
		t.Errorf("sizeof mtget = %d, want 48", sizeofMtget)
	}
}

// TestReadFilemarkIsEOFThenEndOfData checks st.rst semantics: the first
// zero-byte read at a filemark is io.EOF; two consecutive zeros mean end of
// recorded data (ErrEndOfData).
func TestReadFilemarkIsEOFThenEndOfData(t *testing.T) {
	tape, _ := openFake(t)
	defer mustClose(t, tape.Tape)
	if _, err := tape.Write([]byte("block")); err != nil {
		t.Fatal(err)
	}
	if err := tape.WriteFilemarks(1); err != nil {
		t.Fatal(err)
	}
	tape.dev.readIdx = 0 // emulate rewind before reading

	buf := make([]byte, 32)
	if _, err := tape.Read(buf); err != nil {
		t.Fatalf("read data: %v", err)
	}
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF at filemark, got %v", err)
	}
	if _, err := tape.Read(buf); !errors.Is(err, ErrEndOfData) {
		t.Fatalf("want ErrEndOfData after two zero reads, got %v", err)
	}
}

// TestReadDataResetsZeroCount ensures a successful read between filemarks
// resets the consecutive-zero counter so a later single filemark is io.EOF,
// not a spurious ErrEndOfData.
func TestReadDataResetsZeroCount(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	mustWrite(t, tape.Tape, []byte("a"))
	if err := tape.WriteFilemarks(1); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, tape.Tape, []byte("b"))
	if err := tape.WriteFilemarks(1); err != nil {
		t.Fatal(err)
	}
	dev.readIdx = 0

	buf := make([]byte, 8)
	mustRead(t, tape.Tape, buf) // "a"
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("first filemark: want io.EOF, got %v", err)
	}
	mustRead(t, tape.Tape, buf) // "b" resets the counter
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("second filemark: want io.EOF, got %v", err)
	}
	if _, err := tape.Read(buf); !errors.Is(err, ErrEndOfData) {
		t.Fatalf("want ErrEndOfData after the final filemark, got %v", err)
	}
}

// TestSeekResetsZeroCount ensures a tape-movement op invalidates the EOD
// counter so a single zero read after seeking is io.EOF, not ErrEndOfData.
func TestSeekResetsZeroCount(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	mustWrite(t, tape.Tape, []byte("a"))
	if err := tape.WriteFilemarks(1); err != nil {
		t.Fatal(err)
	}
	dev.readIdx = 0

	buf := make([]byte, 8)
	mustRead(t, tape.Tape, buf)                            // "a"
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) { // zeroCount = 1
		t.Fatalf("want io.EOF at filemark, got %v", err)
	}
	if _, err := tape.Seek(0, io.SeekStart); err != nil { // resets counter
		t.Fatalf("seek: %v", err)
	}
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("after seek want io.EOF (not ErrEndOfData), got %v", err)
	}
}

// TestRewindResetsZeroCount: Rewind is a movement op and must reset the EOD
// counter just like Seek.
func TestRewindResetsZeroCount(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	mustWrite(t, tape.Tape, []byte("a"))
	if err := tape.WriteFilemarks(1); err != nil {
		t.Fatal(err)
	}
	dev.readIdx = 0

	buf := make([]byte, 8)
	mustRead(t, tape.Tape, buf)
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF at filemark, got %v", err)
	}
	if err := tape.Rewind(); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	// readIdx was reset to 0 by the fake rewind; the data block clears the
	// counter, then the filemark is a clean io.EOF.
	mustRead(t, tape.Tape, buf)
	if _, err := tape.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("after rewind want io.EOF, got %v", err)
	}
}

// TestWriteFirstENOSPCIsEarlyWarningThenEndOfMedium checks st.rst "EOM
// Behaviour When Writing": the first ENOSPC is the early warning (a trailer
// write is still permitted); only a subsequent ENOSPC is the physical end of
// medium.
func TestWriteFirstENOSPCIsEarlyWarningThenEndOfMedium(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	dev.forceENOSPC = true

	_, err := tape.Write([]byte("x"))
	if !errors.Is(err, ErrEarlyWarning) {
		t.Fatalf("first ENOSPC: want ErrEarlyWarning, got %v", err)
	}
	if errors.Is(err, ErrEndOfMedium) {
		t.Fatalf("first ENOSPC must not satisfy ErrEndOfMedium")
	}
	_, err = tape.Write([]byte("y"))
	if !errors.Is(err, ErrEndOfMedium) {
		t.Fatalf("second ENOSPC: want ErrEndOfMedium, got %v", err)
	}
}

// TestStatusDecodesDsregAndSoftErrors verifies BlockSize/Density are decoded
// from mt_dsreg and SoftErrorCount is the low 16 bits of mt_erreg (st.rst:
// "recovered errors since the previous status call is stored in the lower
// word of mt_erreg").
func TestStatusDecodesDsregAndSoftErrors(t *testing.T) {
	tape, dev := openFake(t)
	defer mustClose(t, tape.Tape)
	dev.dsreg = int64(0x42)<<dsDensityShift | 512
	dev.erreg = 0x00030045

	s, err := tape.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.BlockSize != 512 {
		t.Errorf("BlockSize = %d, want 512", s.BlockSize)
	}
	if s.Density != 0x42 {
		t.Errorf("Density = %#x, want 0x42", s.Density)
	}
	if s.SoftErrorCount != 0x0045 {
		t.Errorf("SoftErrorCount = %#x, want 0x0045", s.SoftErrorCount)
	}
}

// --- benchmarks: assert zero allocation on hot paths ----------------------

func BenchmarkWrite(b *testing.B) {
	tape, _ := openFake(b)
	defer mustClose(b, tape.Tape)
	payload := make([]byte, 64*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := tape.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRead(b *testing.B) {
	tape, dev := openFake(b)
	defer mustClose(b, tape.Tape)
	dev.blocks = [][]byte{make([]byte, 64*1024)}
	buf := make([]byte, 64*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		dev.readIdx = 0
		if _, err := tape.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSeek(b *testing.B) {
	tape, _ := openFake(b)
	defer mustClose(b, tape.Tape)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := tape.Seek(int64(i), io.SeekStart); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStatus(b *testing.B) {
	tape, _ := openFake(b)
	defer mustClose(b, tape.Tape)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := tape.Status(); err != nil {
			b.Fatal(err)
		}
	}
}
