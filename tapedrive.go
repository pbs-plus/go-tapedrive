//go:build linux

package tapedrive

import (
	"errors"
	"io"
	"os"
	"unsafe"
)

// ErrBackwardSeek is returned by Seek when the requested position is before
// the current logical byte offset.
//
// The Linux SCSI tape driver (st(4)) has no byte addressing: it can only
// position by record (MTBSR/MTFSR) or by logical block number (MTSEEK).
// Neither is byte-precise, so a backward byte seek cannot be honored exactly.
// Rewind the drive with Rewind and then Seek forward.
var ErrBackwardSeek = errors.New("tapedrive: backward seek not supported; use Rewind then forward seek")

// DefaultReadBuffer is the size used to fetch records when the drive's own
// block size is unknown or variable. Reads grow the fetch buffer as needed up
// to MaxReadBuffer.
const (
	DefaultReadBuffer = 1 << 20 // 1 MiB
	MaxReadBuffer     = 1 << 26 // 64 MiB (driver max ~2 MiB on most kernels)
)

// recordSource yields one tape record (possibly empty, indicating a filemark
// or end-of-recorded-data boundary) per call. It abstracts the raw read(2) so
// the byte-cursor and Seek math can be tested without hardware.
type recordSource interface {
	// readRecord fills fetch with one record and returns its length. A zero
	// length with nil error marks the end of readable data.
	readRecord(fetch []byte) (int, error)
	// grow suggests a larger fetch capacity when readRecord fails with ENOMEM.
	grow(cap int) []byte
}

// Drive wraps an open SCSI tape device file (e.g. /dev/nst0) and presents a
// byte-oriented, buffered io.ReadSeekCloser. Block boundaries — fixed or
// variable — are hidden: each Read drains bytes from the current record and
// fetches the next record transparently.
//
// Open uses the no-rewind device variant (nst*) so that positioning survives
// Close unless Rewind is called explicitly.
type Drive struct {
	f   *os.File
	src recordSource

	// fetch is the buffer passed to read(2). It holds one record at a time.
	fetch []byte
	// n is the number of valid bytes in fetch after the last read.
	n int
	// off is the read cursor within fetch[0:n].
	off int

	// atEOF is true once read returned 0 at a filemark/EOD boundary.
	atEOF bool
	// pos is the logical byte offset consumed by the caller across all records.
	pos int64

	// rewindOnClose mirrors the no-rewind/auto-rewind device choice.
	rewindOnClose bool
}

// Open opens a no-rewind SCSI tape device for reading.
//
// Use a non-rewinding device (nst0, not st0); with an auto-rewind device the
// tape is rewound to BOT on Close, defeating Seek.
func Open(name string) (*Drive, error) {
	return open(name, os.O_RDONLY, false)
}

// OpenFile is the general constructor. flags must include one of O_RDONLY /
// O_WRONLY / O_RDWR. Set rewindOnClose to rewind the tape when Close is called
// (useful with the auto-rewind device nodes).
func OpenFile(name string, flag int, rewindOnClose bool) (*Drive, error) {
	return open(name, flag, rewindOnClose)
}

func open(name string, flag int, rewindOnClose bool) (*Drive, error) {
	f, err := os.OpenFile(name, flag, 0)
	if err != nil {
		return nil, err
	}
	d := &Drive{
		f:             f,
		src:           &fdSource{f: f},
		fetch:         make([]byte, 0, DefaultReadBuffer),
		rewindOnClose: rewindOnClose,
	}
	// Probe the drive's configured block size. If it is fixed and non-zero we
	// can size the fetch buffer exactly, which avoids the grow-on-demand path.
	if bs, err := d.blockSize(); err == nil && bs > 0 {
		d.fetch = make([]byte, bs)
	}
	return d, nil
}

// Read implements io.Reader. It returns up to len(p) bytes from the current
// record; when a record is exhausted the next record is fetched automatically.
// It returns io.EOF when the end of recorded data (filemark at EOD) is reached.
func (d *Drive) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if d.atEOF {
		return 0, io.EOF
	}

	var total int
	for total < len(p) {
		// Serve any leftover bytes in the current record first.
		if d.off < d.n {
			c := copy(p[total:], d.fetch[d.off:d.n])
			d.off += c
			d.pos += int64(c)
			total += c
			// io.Reader allows short reads; returning here keeps latency low
			// and makes Read yield at record boundaries naturally.
			return total, nil
		}
		// Current record drained: fetch the next one.
		n, err := d.nextRecord()
		if n == 0 {
			if err == nil {
				err = io.EOF
			}
			d.atEOF = true
			if total > 0 {
				return total, nil
			}
			return 0, err
		}
		d.n = n
		d.off = 0
	}
	return total, nil
}

// nextRecord performs one read for a single tape record, growing the fetch
// buffer if the driver needs more space (variable-block or oversized records).
// It returns the record length and any error. A zero length with nil error
// means a filemark / end-of-recorded-data boundary was crossed.
func (d *Drive) nextRecord() (int, error) {
	for {
		n, err := d.src.readRecord(d.fetch[:cap(d.fetch)])
		if err == nil || errors.Is(err, io.EOF) {
			return n, nil
		}
		// ENOMEM: the next physical block is larger than our fetch buffer.
		// Grow and retry (bounded by MaxReadBuffer).
		if isErrno(err, errnoENOMEM) && cap(d.fetch) < MaxReadBuffer {
			d.fetch = d.src.grow(cap(d.fetch))
			continue
		}
		return n, err
	}
}

// Seek implements io.Seeker over logical byte offsets.
//
// Only forward motion is supported: SeekStart/SeekCurrent offsets that move
// the cursor ahead are honored exactly (by reading and discarding), and the
// returned position is a real byte count. Backward seeks return ErrBackwardSeek.
// SeekEnd returns ErrBackwardSeek as well, since the st driver reports no
// meaningful byte length (only block/file numbers).
//
// The driver cannot position by byte; this implementation maintains the byte
// cursor itself and advances it honestly.
func (d *Drive) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = d.pos + offset
	case io.SeekEnd:
		return d.pos, ErrBackwardSeek
	default:
		return d.pos, errors.New("tapedrive: invalid whence")
	}

	if target < 0 {
		return d.pos, ErrBackwardSeek
	}
	if target < d.pos {
		return d.pos, ErrBackwardSeek
	}
	if target == d.pos {
		return d.pos, nil
	}

	// Discard forward the requested delta.
	discard := target - d.pos
	buf := d.fetch[:cap(d.fetch)]
	for discard > 0 {
		if d.atEOF {
			return d.pos, io.EOF
		}
		want := min(int64(len(buf)), discard)
		n, err := d.Read(buf[:want])
		if n > 0 {
			discard -= int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && discard == 0 {
				break
			}
			return d.pos, err
		}
	}
	return d.pos, nil
}

// Position returns the logical byte offset of the next byte to be read.
func (d *Drive) Position() int64 { return d.pos }

// Status returns the raw MTIOCGET status. The GMT* constants in this package
// test individual bits of Gstat (EOF, BOT, EOT, EOD, write-protect, online).
func (d *Drive) Status() (mtget, error) {
	var st mtget
	if err := ioctl(int(d.f.Fd()), mtioCget, uintptr(unsafe.Pointer(&st))); err != nil {
		return mtget{}, err
	}
	return st, nil
}

// BlockSize reports the drive's configured block size from MTIOCGET, or 0 for
// variable-block mode. Best-effort; an error means status could not be read.
func (d *Drive) BlockSize() (int, error) {
	st, err := d.Status()
	if err != nil {
		return 0, err
	}
	return int((st.Dsreg >> dsregBlksizeShift) & dsregBlksizeMask), nil
}

// Rewind rewinds the tape to the beginning of tape (BOT) and resets the byte
// cursor. After Rewind, any buffered record is discarded.
func (d *Drive) Rewind() error {
	if err := d.mtop(mtrew, 1); err != nil {
		return err
	}
	d.resetCursor()
	return nil
}

// Close releases the device. If opened with rewindOnClose the tape is rewound
// first. Per st(4), a filemark is auto-written on close if the last operation
// was a write.
func (d *Drive) Close() error {
	var err error
	if d.rewindOnClose {
		_ = d.Rewind()
	}
	if d.f != nil {
		err = d.f.Close()
		d.f = nil
	}
	return err
}

// File is the underlying *os.File; use it only for operations this package
// does not cover (writing, status queries). Tampering with read position will
// desync the byte cursor.
func (d *Drive) File() *os.File { return d.f }

func (d *Drive) resetCursor() {
	d.n, d.off = 0, 0
	d.pos = 0
	d.atEOF = false
}

// mtop executes an MTIOCTOP command.
func (d *Drive) mtop(op int, count int) error {
	arg := mtop{Op: int16(op), Count: int32(count)}
	return ioctl(int(d.f.Fd()), mtioctop, uintptr(unsafe.Pointer(&arg)))
}

// blockSize queries the drive's current block size from MTIOCGET, returning 0
// for variable-block mode. It is best-effort.
func (d *Drive) blockSize() (int, error) {
	var st mtget
	if err := ioctl(int(d.f.Fd()), mtioCget, uintptr(unsafe.Pointer(&st))); err != nil {
		return 0, err
	}
	return int((st.Dsreg >> dsregBlksizeShift) & dsregBlksizeMask), nil
}

// Compile-time interface satisfaction.
var (
	_ io.ReadSeekCloser = (*Drive)(nil)
)
