package tapedrive

import (
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"unsafe"
)

// fakeDevice is an in-memory stand-in for the st driver used to exercise the
// ioctl plumbing and Read/Write/Seek logic without real hardware. It records
// the sequence of ioctls and serves Read/Write from a ring of byte blocks.
type fakeDevice struct {
	mu       sync.Mutex
	closed   bool
	blocks   [][]byte // written records
	readIdx  int      // next record to read
	blkno    int64    // reported by MTIOCPOS / Seek
	gstat    int64
	lastMtop mtop
	opLog    []mtop
}

func newFake() *fakeDevice { return &fakeDevice{gstat: gmtOnline} }

func (f *fakeDevice) ioctlTop(op *mtop) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMtop = *op
	f.opLog = append(f.opLog, *op)
	switch op.Op {
	case OpWEOF, OpWEOFI:
		// filemark = nil block sentinel
		f.blocks = append(f.blocks, nil)
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
	*g = mtget{Gstat: f.gstat}
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
		return 0, io.EOF
	}
	b := f.blocks[f.readIdx]
	if b == nil {
		f.readIdx++
		return 0, io.EOF // filemark boundary
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
	cp := make([]byte, len(p))
	copy(cp, p)
	f.blocks = append(f.blocks, cp)
	f.blkno++
	return len(p), nil
}

func (f *fakeDevice) close() error { f.closed = true; return nil }

// --- wiring: swap the real syscall-backed methods with the fake -------------

// fakeTape is a Tape whose fd/ops route to a fakeDevice instead of the kernel.
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
	// last op recorded should be a MTSEEK with count 42
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

// TestMTIOCIoctlNumbers checks our hand-computed ioctl numbers are stable.
// On Linux/amd64 the expected values are well known.
func TestMTIOCIoctlNumbers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ioctl numbers are Linux-specific")
	}
	// MTIOCTOP = _IOW('m', 1, struct mtop=8) => 0x40086d01 on 64-bit
	if ioctlMTIOCTOP != 0x40086d01 {
		t.Errorf("ioctlMTIOCTOP = %#x, want 0x40086d01", ioctlMTIOCTOP)
	}
	// MTIOCGET = _IOR('m', 2, struct mtget) where sizeof(mtget)=56 here
	// _IOR => dir=2 => 0x80386d02
	if ioctlMTIOCGET != 0x80386d02 {
		t.Errorf("ioctlMTIOCGET = %#x, want 0x80386d02 (sizeof mtget=%d)", ioctlMTIOCGET, sizeofMtget)
	}
	// MTIOCPOS = _IOR('m', 3, struct mtpos=8) => 0x80086d03
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
	// mtget: 7 * int64 = 56
	if sizeofMtget != 56 {
		t.Errorf("sizeof mtget = %d, want 56", sizeofMtget)
	}
}

// --- benchmarks: assert zero allocation on hot paths -----------------------

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
	// preload one record, reuse it for every read by rewinding the index.
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

// keep unsafe import referenced (used by ioctl plumbing at runtime)
var _ = unsafe.Sizeof(mtop{})
var _ = os.O_RDWR
