//go:build linux

package tapedrive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Drive wraps an open SCSI tape device (st(4)) and exposes a block-oriented
// API mapping directly onto the driver's record/filemark/block-addressing
// model — the right layer for formats defined in terms of tape blocks and
// filemarks, notably Microsoft Tape Format (MTF/.bkf), whose Physical Block
// Address (PBA) scheme assumes device-level block seeking.
//
// A Drive is not an io.Reader/io.Seeker: reads return whole records and seeks
// are by device block number (PBA), not byte offset. This matches st(4), which
// has no byte addressing — only records, filemarks, and logical block numbers.
//
// Use a non-rewinding device (nst0, not st0) so positioning survives Close.
type Drive struct {
	f *os.File

	// buf backs ReadBlock; it grows on demand up to MaxBlockSize when a record
	// is larger than its current capacity.
	buf []byte
	// zerosSeen counts consecutive zero-length reads to distinguish a filemark
	// (one zero read) from end-of-recorded-data (two), per st(4). Reset by any
	// non-zero read or positioning op.
	zerosSeen int
}

// MaxBlockSize bounds the internal read buffer's growth.
const MaxBlockSize = 1 << 26 // 64 MiB

// Open opens a no-rewinding SCSI tape device read-only.
func Open(name string) (*Drive, error) {
	flags := unix.O_RDONLY &^ unix.O_CLOEXEC // some st drivers reject O_CLOEXEC ioctls
	fd, err := unix.Open(name, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("tapedrive: open %s: %w", name, err)
	}
	return &Drive{f: os.NewFile(uintptr(fd), name)}, nil
}

// ReadBlock reads one tape record and returns it. The slice aliases the Drive's
// internal buffer and is valid only until the next ReadBlock call — like
// bufio.Scanner.Bytes; copy it to retain it.
//
// Return values:
//
//	(data, nil)         one record of len(data) bytes
//	(nil, io.EOF)       a filemark was crossed; the head is now just past it,
//	                    at the start of the next file
//	(nil, ErrEndOfData) end of recorded data; no more data on the medium
//	(nil, err)          a genuine I/O error
//
// The filemark-vs-EOD distinction matters for tape formats (MTF) that delimit
// data sets with filemarks: io.EOF means "end of this data set, more may
// follow"; ErrEndOfData means "end of medium". Per st(4) EOD is two consecutive
// zero-length reads; a single zero-length read is a filemark.
func (d *Drive) ReadBlock() ([]byte, error) {
	for {
		if cap(d.buf) == 0 {
			d.buf = make([]byte, 1<<20)
		}
		n, err := unix.Read(int(d.f.Fd()), d.buf[:cap(d.buf)])
		if err != nil {
			if !errors.Is(err, unix.ENOMEM) {
				return nil, err
			}
			if cap(d.buf) >= MaxBlockSize {
				return nil, fmt.Errorf("tapedrive: record exceeds MaxBlockSize (%d)", MaxBlockSize)
			}
			d.buf = make([]byte, min(cap(d.buf)*2, MaxBlockSize))
			continue
		}
		if n > 0 {
			d.zerosSeen = 0
			return d.buf[:n], nil
		}
		d.zerosSeen++
		if d.zerosSeen == 1 {
			return nil, io.EOF // filemark
		}
		return nil, ErrEndOfData
	}
}

// SeekBlock positions the tape at the given Physical Block Address (PBA) via
// the SCSI LOCATE command (MTSEEK). PBA is a device block number as reported
// by TellBlock, not a byte offset; it is bidirectional (forward or backward).
// Requires MTSEEK support (SCSI-2 LOCATE or later).
func (d *Drive) SeekBlock(pba int64) error {
	if err := d.mtop(mtseek, int(pba)); err != nil {
		return fmt.Errorf("tapedrive: seek to block %d: %w", pba, err)
	}
	return nil
}

// TellBlock returns the current Physical Block Address via MTIOCPOS — the same
// value MTF stores in the MTF_SSET DBLK and uses as the anchor for FLA→PBA
// calculations. Requires READ POSITION support (SCSI-2 or later).
func (d *Drive) TellBlock() (int64, error) {
	var pos mtpos
	if err := ioctl(int(d.f.Fd()), mtioCpos, uintptr(unsafe.Pointer(&pos))); err != nil {
		return 0, fmt.Errorf("tapedrive: tell block: %w", err)
	}
	return pos.Blkno, nil
}

// SetLogicalAddressing enables SCSI-2 logical block addressing
// (MT_ST_SCSI2LOGICAL via MTSETDRVBUFFER). Required for stored PBAs — the Last
// ESET PBA in an MTF EOTM block, or the SSETPBA in an MTF_SSET — to be
// meaningful: tape writers record logical PBAs. Idempotent.
func (d *Drive) SetLogicalAddressing() error {
	return d.mtop(mtsetdrvbuf, int(stSetBooleans|stSCSI2Logical))
}

// FSF forward-spaces over count filemarks (advance across MTF data sets, each
// of which begins after a filemark).
func (d *Drive) FSF(count int) error { return d.mtop(mtfsf, count) }

// Status is the typed drive status from MTIOCGET.
type Status struct {
	Online         bool
	BOT            bool // at beginning of tape
	EOT            bool // a tape op reached physical end of tape
	EOD            bool // at end of recorded data
	EOF            bool // positioned just after a filemark
	WriteProtect   bool
	FileNumber     int64 // -1 if unknown
	BlockNumber    int64 // -1 if unknown
	BlockSize      int   // 0 = variable-block mode
	SoftErrorCount int
}

// Status queries MTIOCGET (preceded by MTNOP, which st(4) requires to flush
// the driver buffer before reading status).
func (d *Drive) Status() (Status, error) {
	if err := d.mtop(mtnop, 1); err != nil {
		return Status{}, fmt.Errorf("tapedrive: flush (MTNOP): %w", err)
	}
	var g mtget
	if err := ioctl(int(d.f.Fd()), mtioCget, uintptr(unsafe.Pointer(&g))); err != nil {
		return Status{}, fmt.Errorf("tapedrive: status (MTIOCGET): %w", err)
	}
	return Status{
		Online:         g.Gstat&GMTOnline != 0,
		BOT:            g.Gstat&GMTBOT != 0,
		EOT:            g.Gstat&GMTEOT != 0,
		EOD:            g.Gstat&GMTEOD != 0,
		EOF:            g.Gstat&GMTEOF != 0,
		WriteProtect:   g.Gstat&GMTWRProt != 0,
		FileNumber:     int64(g.Fileno),
		BlockNumber:    int64(g.Blkno),
		BlockSize:      int(uint64(g.Dsreg>>dsregBlksizeShift) & dsregBlksizeMask),
		SoftErrorCount: int(uint64(g.Erreg) & 0xffff),
	}, nil
}

// Rewind rewinds to beginning of tape and blocks until the drive confirms BOT
// via hardware status (GMT_BOT from MTIOCGET). It retries up to 3 times with a
// 250 ms settling delay when the drive hasn't asserted BOT yet — needed after a
// mid-read kill where the firmware may still be draining a prior SCSI command.
// The block counter (MTIOCPOS) is also verified to be 0.
func (d *Drive) Rewind() error {
	for attempt := range 3 {
		if err := d.mtop(mtrew, 1); err != nil {
			return fmt.Errorf("tapedrive: rewind: %w", err)
		}
		pos, posErr := d.TellBlock()
		st, stErr := d.Status()
		if posErr == nil && stErr == nil && pos == 0 && st.BOT {
			return nil
		}
		if attempt < 2 {
			// Pause to let the drive settle. The st driver may queue commands
			// internally; MTREW returns after the command is *accepted*, not
			// after the drive has physically repositioned. 250 ms matches the
			// typical LTO seek-to-BOT time for a mid-tape rewind.
			mtnopWait(d)
		}
	}
	return fmt.Errorf("tapedrive: rewind: drive did not report BOT after 3 attempts")
}

// mtnopWait issues MTNOP to flush the driver buffer and then waits for the
// settle interval. MTNOP is the recommended way to force the st driver to
// synchronise with the hardware before the next MTIOCGET (st(4)).
func mtnopWait(d *Drive) {
	// Best-effort flush; failure is harmless — the retry loop above will catch
	// a drive that genuinely failed to rewind.
	_ = d.mtop(mtnop, 1)
	// Drive firmware may need a few hundred ms to reposition.
	time.Sleep(250 * time.Millisecond)
}

// EOM positions the tape at end of recorded data (for appending).
func (d *Drive) EOM() error { return d.mtop(mteom, 1) }

// Close releases the device file. The tape is not rewound.
func (d *Drive) Close() error {
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// mtop executes an MTIOCTOP command and resets the filemark/EOD tracker.
func (d *Drive) mtop(op int, count int) error {
	arg := mtop{Op: int16(op), Count: int32(count)}
	if err := ioctl(int(d.f.Fd()), mtioctop, uintptr(unsafe.Pointer(&arg))); err != nil {
		return err
	}
	d.zerosSeen = 0
	return nil
}
