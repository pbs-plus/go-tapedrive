// Usage: tdseek <device> [file-no]
//
// It spaces forward file-no files (default 0 = BOT), reads a reference window
// of bytes the "honest" way (sequential ReadAll), then for a set of target
// offsets it seeks there and reads a sample, comparing against the reference.
// Targets include offsets that straddle record boundaries. Each target is
// reached three different ways to test determinism:
//
//	A) SeekStart from the file's start (after a rewind+fsf)
//	B) SeekCurrent in two forward hops
//	C) repeated tiny SeekCurrent steps
//
// All three must yield the identical byte sample, and that sample must equal
// the corresponding slice of the reference window. READ-ONLY; rewinds to BOT.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/pbs-plus/go-tapedrive"
)

const (
	windowSize = 1 << 16 // 64 KiB reference window
	sampleLen  = 32      // bytes read+compared at each target
)

// targets: mix of small, mid, and just-past-power-of-2 to stress boundaries.
var targets = []int64{0, 1, 7, 15, 16, 17, 255, 256, 257, 4095, 4096, 4097, 8191, 8192, 8193, 32767, 32768, 65535}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tdseek <device> [file-no]")
		os.Exit(2)
	}
	dev := os.Args[1]
	fileNo := 0
	if len(os.Args) >= 3 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil {
			fileNo = n
		}
	}

	// A single Drive for the whole run: the st driver only allows one open fd
	// per device, and determinism must hold on ONE handle anyway (rewind +
	// re-seek), which is the realistic caller pattern.
	d, err := tapedrive.Open(dev)
	if err != nil {
		fail("open: %v", err)
	}
	defer shutdown(d)

	// Ensure a known starting position: a prior process may have left the
	// head mid-tape. Open does not rewind.
	if err := d.Rewind(); err != nil {
		fail("initial rewind: %v", err)
	}
	if fileNo > 0 {
		if err := d.FSF(fileNo); err != nil {
			fail("fsf %d: %v", fileNo, err)
		}
	}
	fmt.Printf("testing at file %d\n", fileNo)
	testFileNo = fileNo

	// 1) Build the reference window by sequential read from BOT.
	ref := make([]byte, windowSize)
	nref, err := io.ReadFull(d, ref)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		fail("reference read: %v", err)
	}
	ref = ref[:nref]
	fmt.Printf("reference window: %d bytes\n", nref)
	if bs, _ := d.BlockSize(); bs == 0 {
		fmt.Println("mode: variable-block")
	} else {
		fmt.Printf("mode: fixed-block %d bytes\n", bs)
	}

	// 2) For each target, reach it three independent ways and compare.
	var failed int
	for _, t := range targets {
		if t >= int64(len(ref)) {
			fmt.Printf("target %6d: SKIP (beyond reference window)\n", t)
			continue
		}
		want := ref[t:]
		if int64(len(want)) > sampleLen {
			want = want[:sampleLen]
		}

		a, errA := sampleSeekStart(d, t, len(want))
		b, errB := sampleSeekCurrentHops(d, t, len(want))
		c, errC := sampleSeekCurrentSteps(d, t, len(want))

		ok := errA == nil && errB == nil && errC == nil &&
			bytes.Equal(a, want) && bytes.Equal(b, want) && bytes.Equal(c, want) &&
			bytes.Equal(a, b) && bytes.Equal(b, c)
		status := "OK"
		if !ok {
			status = "FAIL"
			failed++
		}
		fmt.Printf("target %6d: %s  ref=%q  A=%q B=%q C=%q  errA=%v errB=%v errC=%v\n",
			t, status, trunc(want), trunc(a), trunc(b), trunc(c), errA, errB, errC)
	}

	fmt.Println()
	if failed == 0 {
		fmt.Printf("ALL %d TARGETS DETERMINISTIC AND ACCURATE\n", len(targets))
	} else {
		fmt.Printf("%d/%d TARGETS FAILED\n", failed, len(targets))
		os.Exit(1)
	}
}

// rewind resets the single Drive to the start of the file under test
// (BOT + fsf fileNo) and clears buffered state so each path starts from a
// known position on the same handle.
var testFileNo int

func rewind(d *tapedrive.Drive) error {
	if err := d.Rewind(); err != nil {
		return err
	}
	if testFileNo > 0 {
		return d.FSF(testFileNo)
	}
	return nil
}

// sampleSeekStart: rewind, SeekStart(t), read n bytes.
func sampleSeekStart(d *tapedrive.Drive, t int64, n int) ([]byte, error) {
	if err := rewind(d); err != nil {
		return nil, err
	}
	pos, err := d.Seek(t, io.SeekStart)
	if err != nil {
		return nil, err
	}
	if pos != t {
		return nil, fmt.Errorf("SeekStart pos=%d want=%d", pos, t)
	}
	return readExact(d, n)
}

// sampleSeekCurrentHops: rewind, SeekStart(t/2), then SeekCurrent(t-t/2).
func sampleSeekCurrentHops(d *tapedrive.Drive, t int64, n int) ([]byte, error) {
	if err := rewind(d); err != nil {
		return nil, err
	}
	half := t / 2
	if _, err := d.Seek(half, io.SeekStart); err != nil {
		return nil, err
	}
	pos, err := d.Seek(t-half, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if pos != t {
		return nil, fmt.Errorf("hops pos=%d want=%d", pos, t)
	}
	return readExact(d, n)
}

// sampleSeekCurrentSteps: rewind, SeekStart(0), then forward in varied-size
// SeekCurrent steps.
func sampleSeekCurrentSteps(d *tapedrive.Drive, t int64, n int) ([]byte, error) {
	if err := rewind(d); err != nil {
		return nil, err
	}
	if _, err := d.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var i int64
	for i < t {
		step := int64(1)
		if t-i < 7 {
			step = t - i
		}
		pos, err := d.Seek(step, io.SeekCurrent)
		if err != nil {
			return nil, fmt.Errorf("step at %d: %w", i, err)
		}
		i = pos
	}
	if i != t {
		return nil, fmt.Errorf("steps landed at %d want %d", i, t)
	}
	return readExact(d, n)
}

func readExact(d *tapedrive.Drive, n int) ([]byte, error) {
	out := make([]byte, n)
	got, err := io.ReadFull(d, out)
	if got == n {
		return out, nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return out[:got], nil
	}
	return out[:got], err
}

func trunc(b []byte) string {
	if len(b) > 12 {
		b = b[:12]
	}
	return string(b)
}

func shutdown(d *tapedrive.Drive) {
	_ = d.Rewind()
	_ = d.Close()
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "tdseek: "+format+"\n", a...)
	os.Exit(1)
}
