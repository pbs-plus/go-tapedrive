// Block-oriented integration probe against a real st(4) drive.
//
// Usage: tdblock <device>
//
// READ-ONLY. Exercises the block API: Status, BlockSize, ReadBlock across a
// filemark, FSF to the next Data Set, and — the MTF-critical pair —
// TellBlock + SeekBlock round-trip for hardware-accelerated random access.
// Rewinds to BOT on exit.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pbs-plus/go-tapedrive"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tdblock <device>")
		os.Exit(2)
	}
	dev := os.Args[1]

	d, err := tapedrive.Open(dev)
	if err != nil {
		fail("open: %v", err)
	}
	defer func() {
		_ = d.Rewind()
		_ = d.Close()
	}()
	if err := d.Rewind(); err != nil {
		fail("rewind: %v", err)
	}

	// --- Status ---
	st, err := d.Status()
	if err != nil {
		fail("status: %v", err)
	}
	fmt.Printf("status: online=%v bot=%v eod=%v wr_prot=%v fileno=%d blkno=%d blocksize=%d (%s)\n",
		st.Online, st.BOT, st.EOD, st.WriteProtect, st.FileNumber, st.BlockNumber, st.BlockSize,
		modeStr(st.BlockSize))

	// --- Read first block, record its PBA ---
	pba0, err := d.TellBlock()
	if err != nil {
		fmt.Printf("(TellBlock unsupported on this drive: %v)\n", err)
	} else {
		fmt.Printf("tell: PBA before first read = %d\n", pba0)
	}
	blk0, err := d.ReadBlock()
	if err != nil {
		fail("first ReadBlock: %v", err)
	}
	fmt.Printf("read: first block %d bytes: %q\n", len(blk0), trunc(blk0))
	pba1, err := d.TellBlock()
	if err == nil {
		fmt.Printf("tell: PBA after first read = %d (delta %d)\n", pba1, pba1-pba0)
	}

	// --- Read to end of first file (Data Set) ---
	var nblocks int
	for {
		_, err := d.ReadBlock()
		if err == nil {
			nblocks++
			continue
		}
		if errors.Is(err, io.EOF) {
			fmt.Printf("read: hit filemark after %d more blocks (end of Data Set 0)\n", nblocks)
			break
		}
		if errors.Is(err, tapedrive.ErrEndOfData) {
			fmt.Printf("read: hit EOD (single Data Set tape)\n")
			break
		}
		fmt.Printf("read: error scanning file 0: %v\n", err)
		break
	}

	// --- FSF into the next Data Set, read its first block ---
	if err := d.FSF(1); err != nil {
		fmt.Printf("fsf: %v (no further Data Sets, or unsupported)\n", err)
	} else {
		blk, err := d.ReadBlock()
		if err != nil {
			fmt.Printf("read after fsf: %v\n", err)
		} else {
			fmt.Printf("read: Data Set 1 first block %d bytes: %q\n", len(blk), trunc(blk))
		}
	}

	// --- The MTF-critical test: SeekBlock back to PBA 0, re-read, compare ---
	if pba0 >= 0 && err == nil {
		fmt.Printf("seek: returning to PBA %d via MTSEEK...\n", pba0)
		if err := d.SeekBlock(pba0); err != nil {
			fmt.Printf("seek: %v (MTSEEK unsupported on this drive)\n", err)
		} else {
			blk, err := d.ReadBlock()
			if err != nil {
				fmt.Printf("read after seek: %v\n", err)
			} else if string(blk) == string(blk0) {
				fmt.Println("seek: OK — block at PBA 0 matches first read (random access verified)")
			} else {
				fmt.Printf("seek: MISMATCH — got %q, want %q\n", trunc(blk), trunc(blk0))
			}
		}
	}

	fmt.Println("OK")
}

func modeStr(bs int) string {
	if bs == 0 {
		return "variable-block"
	}
	return "fixed-block"
}

func trunc(b []byte) string {
	if len(b) > 32 {
		b = b[:32]
	}
	return string(b)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "tdblock: "+format+"\n", a...)
	os.Exit(1)
}
