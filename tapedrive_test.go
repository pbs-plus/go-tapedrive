package tapedrive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// fakeSource serves a fixed list of records (each possibly different size) and
// then returns a zero-length record (filemark / EOD) with nil error, matching
// the st driver's "two consecutive zero reads = EOD" model simplified to one.
type fakeSource struct {
	records [][]byte
	i       int
	grown   int
}

func (s *fakeSource) readRecord(fetch []byte) (int, error) {
	if s.i >= len(s.records) {
		return 0, nil // end of recorded data
	}
	rec := s.records[s.i]
	if len(rec) > len(fetch) {
		return 0, fmt.Errorf("%w (block %d > fetch %d)", errnoENOMEM, len(rec), len(fetch))
	}
	n := copy(fetch, rec)
	s.i++
	return n, nil
}

func (s *fakeSource) grow(cap int) []byte {
	s.grown++
	next := min(cap*2, MaxReadBuffer)
	return make([]byte, next)
}

func newTestDrive(records [][]byte) *Drive {
	return &Drive{
		src:   &fakeSource{records: records},
		fetch: make([]byte, 0, 8),
	}
}

func TestRead_VariableBlocks(t *testing.T) {
	// Three records of sizes 5, 13, 3 — exercises arbitrary block boundaries.
	records := [][]byte{
		[]byte("hello"),
		[]byte(", brave world"),
		[]byte("!"),
	}
	want := []byte("hello, brave world!")
	d := newTestDrive(records)

	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("data mismatch:\n got %q\nwant %q", got, want)
	}
	if d.Position() != int64(len(want)) {
		t.Fatalf("Position = %d, want %d", d.Position(), len(want))
	}
}

func TestRead_AutoGrowsOnENOMEM(t *testing.T) {
	// Start with a tiny fetch buffer; the 32-byte record forces a grow.
	big := bytes.Repeat([]byte("Z"), 32)
	d := newTestDrive([][]byte{big})
	d.fetch = make([]byte, 0, 4) // smaller than the record

	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("data mismatch: got %d bytes, want %d", len(got), len(big))
	}
	if fs, ok := d.src.(*fakeSource); !ok || fs.grown == 0 {
		t.Fatalf("expected fetch buffer to grow, grew=%d", func() int {
			if s, ok := d.src.(*fakeSource); ok {
				return s.grown
			}
			return 0
		}())
	}
}

func TestRead_ShortReadsAtRecordBoundaries(t *testing.T) {
	// 1-byte reads should still traverse multi-record content.
	records := [][]byte{[]byte("AB"), []byte("CDE")}
	d := newTestDrive(records)
	var out []byte
	buf := make([]byte, 1)
	for {
		n, err := d.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if n == 0 {
			t.Fatal("Read returned 0, nil")
		}
	}
	if string(out) != "ABCDE" {
		t.Fatalf("got %q, want %q", out, "ABCDE")
	}
}

func TestSeek_ForwardExact(t *testing.T) {
	records := [][]byte{[]byte("0123456789"), []byte("ABCDEF")}
	d := newTestDrive(records)

	// Seek 5 bytes forward from start: should land on '5'.
	pos, err := d.Seek(5, io.SeekStart)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if pos != 5 {
		t.Fatalf("pos = %d, want 5", pos)
	}
	buf := make([]byte, 4)
	n, _ := d.Read(buf)
	if string(buf[:n]) != "5678" {
		t.Fatalf("read after seek = %q, want %q", buf[:n], "5678")
	}

	// Seek 2 more bytes forward across the record boundary: lands on 'C'.
	pos, err = d.Seek(2, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek current: %v", err)
	}
	if pos != 11 { // consumed 5 + 4 + 2
		t.Fatalf("pos = %d, want 11", pos)
	}
	n, _ = d.Read(buf)
	if string(buf[:n]) != "BCDE" {
		t.Fatalf("read after relative seek = %q, want %q", buf[:n], "BCDE")
	}
}

func TestSeek_BackwardRejected(t *testing.T) {
	d := newTestDrive([][]byte{[]byte("abcdef")})
	if _, err := d.Seek(3, io.SeekStart); err != nil {
		t.Fatalf("initial seek: %v", err)
	}
	_, err := d.Seek(0, io.SeekStart)
	if !errors.Is(err, ErrBackwardSeek) {
		t.Fatalf("backward SeekStart err = %v, want ErrBackwardSeek", err)
	}
	_, err = d.Seek(-1, io.SeekCurrent)
	if !errors.Is(err, ErrBackwardSeek) {
		t.Fatalf("backward SeekCurrent err = %v, want ErrBackwardSeek", err)
	}
	_, err = d.Seek(0, io.SeekEnd)
	if !errors.Is(err, ErrBackwardSeek) {
		t.Fatalf("SeekEnd err = %v, want ErrBackwardSeek", err)
	}
}

func TestSeek_NoOpSameOffset(t *testing.T) {
	d := newTestDrive([][]byte{[]byte("abcdef")})
	pos, err := d.Seek(3, io.SeekStart)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	pos2, err := d.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("no-op seek: %v", err)
	}
	if pos != pos2 {
		t.Fatalf("pos changed from %d to %d on no-op", pos, pos2)
	}
}

func TestSeek_PastEOD(t *testing.T) {
	d := newTestDrive([][]byte{[]byte("ab")})
	_, err := d.Seek(10, io.SeekStart)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if d.Position() != 2 {
		t.Fatalf("Position = %d, want 2 (should have consumed all available)", d.Position())
	}
}
