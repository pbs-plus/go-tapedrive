// Integration test against a real st(4) drive. Build with:
//
//	GOOS=linux GOARCH=amd64 go build -o tdtest ./cmd/tdtest
//
// Run on the host: ./tdtest /dev/nst0
//
// It performs READ-ONLY operations: opens the no-rewind device O_RDONLY,
// queries status/block size, reads a bounded number of bytes, exercises
// forward Seek, verifies ErrBackwardSeek, then rewinds to BOT and closes.
// It does NOT write anything.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pbs-plus/go-tapedrive"
)

const maxRead = 1 << 14 // 16 KiB ceiling for the probe — keeps head movement minimal

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tdtest <device>")
		os.Exit(2)
	}
	dev := os.Args[1]

	d, err := tapedrive.Open(dev)
	if err != nil {
		fail("open %s: %v", dev, err)
	}
	defer func() {
		// Always rewind to BOT so we leave the tape in a known position.
		if err := d.Rewind(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: rewind: %v\n", err)
		}
		if err := d.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: close: %v\n", err)
		}
	}()

	// --- status (no head movement) ---
	st, err := d.Status()
	if err != nil {
		fail("status: %v", err)
	}
	bs, err := d.BlockSize()
	if err != nil {
		fail("blocksize: %v", err)
	}
	fmt.Printf("open: %s\n", dev)
	fmt.Printf("status: gstat=0x%08x  fileno=%d blkno=%d\n", uint32(st.Gstat), st.Fileno, st.Blkno)
	fmt.Printf("  online=%v bot=%v eot=%v eod=%v eof=%v wr_prot=%v\n",
		st.Gstat&tapedrive.GMTOnline != 0,
		st.Gstat&tapedrive.GMTBOT != 0,
		st.Gstat&tapedrive.GMTEOT != 0,
		st.Gstat&tapedrive.GMTEOD != 0,
		st.Gstat&tapedrive.GMTEOF != 0,
		st.Gstat&tapedrive.GMTWRProt != 0,
	)
	fmt.Printf("blocksize: %d  (%s mode)\n", bs, modeStr(bs))

	// --- read a bounded number of bytes ---
	buf := make([]byte, maxRead)
	total, err := io.ReadFull(d, buf)
	readErr := err
	if readErr == io.ErrUnexpectedEOF {
		readErr = nil // partial read at end of available data is fine
	}
	fmt.Printf("read: %d bytes\n", total)
	if total > 0 {
		show := min(total, 64)
		fmt.Printf("  first %d bytes (hex): %x\n", show, buf[:show])
		fmt.Printf("  first %d bytes (asc): %q\n", show, string(buf[:show]))
	}
	fmt.Printf("position after read: %d\n", d.Position())
	if readErr != nil {
		fmt.Printf("read error: %v\n", readErr)
	}

	// --- forward seek, verify Position ---
	pos, err := d.Seek(7, io.SeekStart)
	if err != nil {
		fmt.Printf("seek(start,7) error: %v\n", err)
	} else {
		fmt.Printf("seek(start,7) -> position=%d\n", pos)
	}
	pos2, err := d.Seek(3, io.SeekCurrent)
	if err != nil {
		fmt.Printf("seek(current,+3) error: %v\n", err)
	} else {
		fmt.Printf("seek(current,+3) -> position=%d\n", pos2)
	}

	// --- backward seek must be rejected ---
	_, err = d.Seek(0, io.SeekStart)
	if errors.Is(err, tapedrive.ErrBackwardSeek) {
		fmt.Println("backward seek correctly rejected with ErrBackwardSeek")
	} else {
		fmt.Printf("UNEXPECTED: backward seek err = %v\n", err)
	}

	fmt.Println("OK")
}

func modeStr(bs int) string {
	if bs == 0 {
		return "variable"
	}
	return "fixed"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "tdtest: "+format+"\n", a...)
	os.Exit(1)
}
