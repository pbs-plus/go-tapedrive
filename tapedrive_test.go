package tapedrive

import (
	"errors"
	"io"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeReader serves a scripted sequence of read results (records as byte
// slices, zero-length for filemarks, or errors). It models st(2) semantics:
// one record per call, zero length at a filemark, ENOMEM if dst < record.
type fakeReader struct {
	script []fakeRead
	i      int
}

type fakeRead struct {
	data []byte // record bytes; nil + zeroErr = filemark (zero read)
	err  error  // non-nil error to return (e.g. unix.ENOMEM)
}

func (r *fakeReader) read(dst []byte) (int, error) {
	if r.i >= len(r.script) {
		return 0, ErrEndOfData
	}
	fr := r.script[r.i]
	if fr.err != nil {
		// ENOMEM special case: do not advance, so retry-with-grow re-reads it.
		if !errors.Is(fr.err, unix.ENOMEM) {
			r.i++
		}
		return 0, fr.err
	}
	if fr.data == nil {
		r.i++
		return 0, nil // filemark (zero-length read)
	}
	if len(fr.data) > len(dst) {
		return 0, unix.ENOMEM // don't advance; caller grows and retries
	}
	r.i++
	return copy(dst, fr.data), nil
}

func newDrive(fr *fakeReader) *Drive {
	d := &Drive{buf: make([]byte, 0, 8)}
	d.read = fr.read
	return d
}

func TestReadBlock_OneRecord(t *testing.T) {
	fr := &fakeReader{script: []fakeRead{{data: []byte("hello")}}}
	d := newDrive(fr)

	got, err := d.ReadBlock()
	if err != nil {
		t.Fatalf("ReadBlock: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestReadBlock_FilemarkThenNextFile(t *testing.T) {
	// file0 "AB", filemark, file1 "CD", filemark, EOD (implicit).
	fr := &fakeReader{script: []fakeRead{
		{data: []byte("AB")},
		{}, // filemark
		{data: []byte("CD")},
		{}, // filemark
		{}, // EOD (second zero in a row, but reset after CD; this is the first zero of a new sequence)
	}}
	d := newDrive(fr)

	// Block 0: "AB"
	b, err := d.ReadBlock()
	if err != nil || string(b) != "AB" {
		t.Fatalf("read0: b=%q err=%v", b, err)
	}
	// Next: filemark → io.EOF
	b, err = d.ReadBlock()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read1: want io.EOF, got b=%q err=%v", b, err)
	}
	// Next: "CD" (next file)
	b, err = d.ReadBlock()
	if err != nil || string(b) != "CD" {
		t.Fatalf("read2: b=%q err=%v", b, err)
	}
	// Next: filemark → io.EOF
	b, err = d.ReadBlock()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read3: want io.EOF, got b=%q err=%v", b, err)
	}
}

func TestReadBlock_EODIsTwoZeros(t *testing.T) {
	// Single record then two consecutive zero reads → first is EOF, second is EOD.
	fr := &fakeReader{script: []fakeRead{
		{data: []byte("X")},
		{}, // filemark
		{}, // EOD
	}}
	d := newDrive(fr)

	if _, err := d.ReadBlock(); err != nil {
		t.Fatalf("read0: %v", err)
	}
	_, err := d.ReadBlock()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read1: want io.EOF (filemark), got %v", err)
	}
	_, err = d.ReadBlock()
	if !errors.Is(err, ErrEndOfData) {
		t.Fatalf("read2: want ErrEndOfData, got %v", err)
	}
}

func TestReadBlock_GrowsOnENOMEM(t *testing.T) {
	// 32-byte record with an 8-byte internal buffer → ENOMEM, grow, retry.
	big := make([]byte, 32)
	for i := range big {
		big[i] = 'Z'
	}
	fr := &fakeReader{script: []fakeRead{{data: big}}}
	d := newDrive(fr)

	got, err := d.ReadBlock()
	if err != nil {
		t.Fatalf("ReadBlock: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("got %d bytes, want 32", len(got))
	}
}

func TestReadBlockInto_ShortBuffer(t *testing.T) {
	fr := &fakeReader{script: []fakeRead{{data: []byte("too long for this buf")}}}
	d := newDrive(fr)

	buf := make([]byte, 4)
	_, err := d.ReadBlockInto(buf)
	if !errors.Is(err, ErrShortBuffer) {
		t.Fatalf("want ErrShortBuffer, got %v", err)
	}
}

func TestReadBlock_PositioningResetsZeroCounter(t *testing.T) {
	// After a filemark (zerosSeen=1), a positioning op must reset the counter
	// so a subsequent single zero read is a fresh filemark (io.EOF), not EOD.
	fr := &fakeReader{script: []fakeRead{
		{data: []byte("A")},
		{}, // filemark
		{data: []byte("B")},
		{}, // filemark again
	}}
	d := newDrive(fr)

	if _, err := d.ReadBlock(); err != nil {
		t.Fatalf("read A: %v", err)
	}
	if _, err := d.ReadBlock(); !errors.Is(err, io.EOF) {
		t.Fatalf("want filemark EOF, got %v", err)
	}
	// Simulate a positioning op resetting the zero counter.
	d.zerosSeen = 0
	if b, err := d.ReadBlock(); err != nil || string(b) != "B" {
		t.Fatalf("read B: got %q err=%v", b, err)
	}
	if _, err := d.ReadBlock(); !errors.Is(err, io.EOF) {
		t.Fatalf("after reset, single zero must be EOF (filemark), got %v", err)
	}
}
