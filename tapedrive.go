//go:build linux

package tapedrive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Drive wraps an open SCSI tape device (st(4)) and exposes a block-oriented
// API that maps directly onto the driver's record/filemark/block-addressing
// model. It is the right layer for formats whose structure is defined in terms
// of tape blocks and filemarks — notably Microsoft Tape Format (MTF/.bkf),
// whose Physical Block Address (PBA) and Format Logical Address (FLA) scheme
// assumes device-level block seeking.
//
// A Drive is NOT an io.Reader/io.Seeker. Reads return whole records; seeks are
// by device block number (PBA), not byte offset. This matches the st driver,
// which has no byte addressing — only records (MTFSR/MTBSR), filemarks
// (MTFSF/MTBSF), and logical block numbers (MTSEEK/MTIOCPOS).
//
// Use a non-rewinding device (nst0, not st0) so positioning survives Close.
type Drive struct {
	f *os.File

	// buf is the internal buffer used by ReadBlock. It grows on demand up to
	// MaxBlockSize when a record is larger than the current capacity.
	buf []byte
	// zerosSeen tracks consecutive zero-length reads to distinguish a filemark
	// (one zero read) from end-of-recorded-data (two zero reads), per st(4).
	// Reset to 0 whenever a non-zero record is read or the head is moved.
	zerosSeen int

	// read issues one driver read into dst, returning (n, err). Defaults to the
	// raw syscall; overridable for tests. Mirrors read(2) on st: one record per
	// call, zero length at a filemark, ENOMEM if dst < next record.
	read func(dst []byte) (int, error)
}

// MaxBlockSize bounds the internal read buffer growth. The st driver's own
// buffer is the real ceiling (~2 MB on most kernels); this is a safety cap.
const MaxBlockSize = 1 << 26 // 64 MiB

// Open opens a no-rewind SCSI tape device read-only.
//
// Use a non-rewinding device (nst*); with an auto-rewind device the tape is
// rewound to BOT on Close.
func Open(name string) (*Drive, error) {
	return open(name, unix.O_RDONLY, 0)
}

// OpenFile opens a tape device with arbitrary flags. flags must include one of
// O_RDONLY/O_WRONLY/O_RDWR. mode is the file mode (usually 0).
func OpenFile(name string, flags, mode int) (*Drive, error) {
	return open(name, flags, mode)
}

func open(name string, flags, mode int) (*Drive, error) {
	// Open WITHOUT O_CLOEXEC: some st drivers reject ioctls on fds opened with
	// O_CLOEXEC. Strip it and rely on explicit Close.
	flags &^= unix.O_CLOEXEC
	fd, err := unix.Open(name, flags, uint32(mode))
	if err != nil {
		return nil, fmt.Errorf("tapedrive: open %s: %w", name, err)
	}
	d := &Drive{f: os.NewFile(uintptr(fd), name)}
	d.read = func(dst []byte) (int, error) { return unix.Read(int(d.f.Fd()), dst) }
	return d, nil
}

// ReadBlock reads one tape record (one variable- or fixed-size block) into buf
// and returns its length. The returned slice view buf[:n] holds exactly one
// record; n is the record's true size as reported by the driver.
//
// If buf is too small for the next record, ReadBlock grows the Drive's internal
// buffer and reads into that instead, returning a slice that aliases the
// internal buffer. Callers that need a stable copy should copy out of it before
// the next ReadBlock. To guarantee reading into your own buf, size it >=
// BlockSize() (fixed-block mode) or >= MaxBlockSize (variable-block mode).
//
// Return values:
//
//	(nil, nil)          unreachable
//	(buf[:n], nil)      one record of n bytes
//	(nil, io.EOF)       a filemark was crossed; no data returned. The head is
//	                    now positioned just past the filemark, at the start of
//	                    the next file. Another ReadBlock reads the next file.
//	(nil, ErrEndOfData) end of recorded data (two filemarks / EOD). No more
//	                    data follows; further reads continue to return this.
//	(nil, err)          a genuine I/O error.
//
// The filemark-vs-EOD distinction matters for tape formats (MTF) that delimit
// Data Sets with filemarks: a single io.EOF means "end of this Data Set, more
// may follow", while ErrEndOfData means "end of medium".
// ReadBlock reads one tape record (one variable- or fixed-size block) and
// returns it. The returned slice aliases the Drive's internal buffer and is
// valid only until the next ReadBlock/ReadBlockInto call — like
// bufio.Scanner.Bytes. Copy it if you need to retain it.
//
// Return values:
//
//	(data, nil)          one record of len(data) bytes
//	(nil, io.EOF)        a filemark was crossed; no data. The head is now just
//	                     past the filemark at the start of the next file; a
//	                     subsequent ReadBlock reads the next file's first block.
//	(nil, ErrEndOfData)  end of recorded data (EOD); no more data on medium.
//	(nil, err)           a genuine I/O error.
//
// The filemark-vs-EOD distinction matters for tape formats (MTF) that delimit
// Data Sets with filemarks: io.EOF = "end of this Data Set, more may follow",
// ErrEndOfData = "end of medium". Per st(4), EOD is two consecutive
// zero-length reads; a single zero-length read is a filemark.
func (d *Drive) ReadBlock() ([]byte, error) {
	n, err := d.readInto(d.buffer())
	if err != nil || n == 0 {
		return nil, err
	}
	return d.buf[:n], nil
}

// ReadBlockInto reads one record into buf and returns its length n. If buf is
// too small for the next record, it returns ErrShortBuffer so the caller can
// retry with a larger buffer (the required size is unknowable in advance in
// variable-block mode; use BlockSize() for fixed-block mode, or ReadBlock for
// auto-sized reads). See ReadBlock for the filemark/EOD error semantics.
func (d *Drive) ReadBlockInto(buf []byte) (int, error) {
	return d.readInto(buf)
}

// buffer returns the internal read buffer, allocating on first use.
func (d *Drive) buffer() []byte {
	if cap(d.buf) == 0 {
		d.buf = make([]byte, 1<<20)[:0] // 1 MiB initial
	}
	return d.buf[:cap(d.buf)]
}

// readInto issues one read(2) into dst and classifies the result. A non-zero
// read is a data record. A zero read is a boundary: the first is reported as
// io.EOF (filemark), and if the NEXT call also reads zero, that is reported as
// ErrEndOfData (EOD) — matching st(4)'s two-zero-read EOD rule. Any non-zero
// read resets the zero counter.
//
// On ENOMEM (dst too small for the next record): if dst is the Drive's own
// buffer, grow and retry; otherwise return ErrShortBuffer.
func (d *Drive) readInto(dst []byte) (int, error) {
	for {
		n, err := d.read(dst)
		if err != nil {
			if !errors.Is(err, unix.ENOMEM) {
				return 0, err
			}
			// ENOMEM: dst too small. Grow only if dst is the internal buffer.
			if isInternal, grown := d.maybeGrow(dst); isInternal {
				if !grown {
					return 0, fmt.Errorf("tapedrive: record exceeds MaxBlockSize (%d)", MaxBlockSize)
				}
				dst = d.buf[:cap(d.buf)]
				continue // retry same record with bigger buffer
			}
			return 0, ErrShortBuffer
		}
		if n > 0 {
			d.zerosSeen = 0
			return n, nil
		}
		// Zero-length read: filemark or EOD.
		d.zerosSeen++
		if d.zerosSeen == 1 {
			return 0, io.EOF // filemark
		}
		return 0, ErrEndOfData // second zero in a row
	}
}

// maybeGrow reports whether dst is the Drive's internal buffer and, if so,
// doubles its capacity (up to MaxBlockSize). Returns (isInternal, didGrow).
func (d *Drive) maybeGrow(dst []byte) (bool, bool) {
	// dst is internal if it shares d.buf's backing array.
	if len(dst) == 0 || cap(d.buf) == 0 {
		return false, false
	}
	if &dst[0] != &d.buf[:cap(d.buf)][0] {
		return false, false
	}
	if cap(d.buf) >= MaxBlockSize {
		return true, false
	}
	d.buf = make([]byte, growCap(cap(d.buf)))
	return true, true
}

func growCap(c int) int {
	next := min(c*2, MaxBlockSize)
	return next
}

// SeekBlock positions the tape at the given Physical Block Address (PBA) using
// the SCSI LOCATE command (MTSEEK). This is MTF's primary random-access
// primitive: the spec's restore formula yields a PBA, and SeekBlock lands
// there in a single hardware operation — far faster than sequential reads.
//
// The PBA is a device block number as reported by TellBlock/MTIOCPOS, NOT a
// byte offset. PBAs are bidirectional: SeekBlock may seek forward or backward.
// Not all drives support LOCATE; check Status for the drive's capabilities if
// unsure. Requires the drive to support MTSEEK (SCSI-2 LOCATE or later).
func (d *Drive) SeekBlock(pba int64) error {
	if err := d.mtop(mtseek, int(pba)); err != nil {
		return fmt.Errorf("tapedrive: seek to block %d: %w", pba, err)
	}
	return nil
}

// TellBlock returns the current Physical Block Address (PBA) via MTIOCPOS.
// This is the device's notion of position — the same value MTF stores in the
// MTF_SSET DBLK and uses as the anchor for FLA→PBA calculations.
//
// Requires a drive that supports READ POSITION (SCSI-2 or later). Returns an
// error if the drive cannot report position.
func (d *Drive) TellBlock() (int64, error) {
	var pos mtpos
	if err := ioctl(int(d.f.Fd()), mtioCpos, uintptr(unsafe.Pointer(&pos))); err != nil {
		return 0, fmt.Errorf("tapedrive: tell block: %w", err)
	}
	return pos.Blkno, nil
}

// FSF forward-spaces over count filemarks. Use to advance across Data Sets
// (MTF: each Data Set begins after a filemark).
func (d *Drive) FSF(count int) error { return d.mtop(mtfsf, count) }

// BSF backward-spaces over count filemarks.
func (d *Drive) BSF(count int) error { return d.mtop(mtbsf, count) }

// FSR forward-spaces over count records (blocks).
func (d *Drive) FSR(count int) error { return d.mtop(mtfsr, count) }

// BSR backward-spaces over count records (blocks).
func (d *Drive) BSR(count int) error { return d.mtop(mtbsr, count) }

// Status reports drive status as a typed struct. Per st(4), MTIOCGET must be
// preceded by MTNOP to flush the driver buffer and refresh the status bits,
// so Status issues both. Note: at BOT, before any media access, the
// GMTWriteProtect bit may not reflect the physical tab reliably.
type Status struct {
	Online         bool  // tape loaded and ready
	BOT            bool  // at beginning of tape
	EOT            bool  // a tape op reached physical end of tape
	EOD            bool  // at end of recorded data
	EOF            bool  // positioned just after a filemark
	WriteProtect   bool  // medium is write-protected
	FileNumber     int64 // current file number (-1 if unknown)
	BlockNumber    int64 // block number within current file (-1 if unknown)
	BlockSize      int   // configured block size; 0 = variable-block mode
	SoftErrorCount int   // recovered error count (often not maintained)
}

// Status queries MTIOCGET (preceded by MTNOP) and returns a typed Status.
func (d *Drive) Status() (Status, error) {
	// MTNOP flushes the buffer; required before MTIOCGET or the kernel may
	// return EINVAL.
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

// Rewind rewinds the tape to beginning of tape (BOT).
func (d *Drive) Rewind() error { return d.mtop(mtrew, 1) }

// EOM positions the tape at the end of recorded data (for appending).
func (d *Drive) EOM() error { return d.mtop(mteom, 1) }

// WriteFilemark writes count filemarks. Write-only; appends a Data Set
// boundary.
func (d *Drive) WriteFilemark(count int) error { return d.mtop(mtweof, count) }

// Close releases the device file. Per st(4), a filemark is auto-written on
// close if the last operation was a write. The tape is NOT rewound (use a
// non-rewinding device and Rewind explicitly when desired).
func (d *Drive) Close() error {
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// File returns the underlying *os.File for operations this package does not
// cover. Tampering with read position or issuing raw ioctls will desync the
// Drive's view of tape state.
func (d *Drive) File() *os.File { return d.f }

// mtop executes an MTIOCTOP command. Any positioning op invalidates the
// filemark/EOD tracking state.
func (d *Drive) mtop(op int, count int) error {
	arg := mtop{Op: int16(op), Count: int32(count)}
	if err := ioctl(int(d.f.Fd()), mtioctop, uintptr(unsafe.Pointer(&arg))); err != nil {
		return err
	}
	d.zerosSeen = 0
	return nil
}
