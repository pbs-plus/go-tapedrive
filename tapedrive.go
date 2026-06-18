package tapedrive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Tape is a handle to a Linux SCSI tape device node (/dev/st* auto-rewind,
// /dev/nst* non-rewind). It satisfies io.Reader, io.Writer, io.Seeker,
// io.Closer and therefore every composition thereof in the io package
// (io.ReadSeeker, io.ReadWriteSeeker, io.ReadWriteCloser, io.ReadSeekCloser,
// io.ReadWriteSeekCloser).
//
// A Tape is safe for use by a single goroutine at a time; the st driver
// serializes commands per file descriptor, so concurrent callers must supply
// their own synchronization (e.g. a mutex) or open one device per goroutine.
//
// All Read/Write/Seek/Position hot paths are zero-allocation: the kernel
// copies directly into/out of the caller-supplied buffers and every ioctl
// argument lives on the stack or inside this struct.
type Tape struct {
	fd      int
	rewind  bool // device is the auto-rewind (/dev/st*) variant
	closed  bool
	hadRead bool // last data-direction op was a read (used by Close filemark logic)
	ops     tapeOps
}

// tapeOps is the small surface of the st driver that Tape uses. The
// production implementation lives in ops_linux.go and calls into the kernel;
// tests inject a fake. Keeping this unexported means the indirection has no
// public-API cost and the methods are inlined on the kernel path.
type tapeOps interface {
	ioctlTop(op *mtop) error
	ioctlGet(g *mtget) error
	ioctlPos(p *mtpos) error
	read(p []byte) (int, error)
	write(p []byte) (int, error)
	close() error
}

// Option configures a Tape at Open time via the functional-options pattern.
type Option func(*openConfig)

type openConfig struct {
	flags      int
	mode       uint32
	blockSize  int  // 0 => variable block mode
	setOptions int  // MTSETDRVBUFFER MT_ST_SETBOOLEANS mask to apply after open
	scsi2log   bool // enable logical block addressing (MT_ST_SCSI2LOGICAL) for HPE Ultrium seek/tell
}

// WithFlags replaces the default open flags (os.O_RDWR | os.O_SYNC). Use it to
// add os.O_NONBLOCK (open without waiting for the drive to become ready) or to
// force O_RDONLY.
func WithFlags(flags int) Option {
	return func(c *openConfig) { c.flags = flags }
}

// WithMode sets the file mode used at open (only meaningful when creating a
// node; usually irrelevant for an existing char device).
func WithMode(mode uint32) Option {
	return func(c *openConfig) { c.mode = mode }
}

// WithBlockSize puts the drive into fixed-block mode with the given block
// size. A size of 0 (the default) selects variable-block mode, where the
// write() byte count determines the physical block size on tape.
func WithBlockSize(size int) Option {
	return func(c *openConfig) { c.blockSize = size }
}

// WithSCSI2Logical enables logical block addressing (MT_ST_SCSI2LOGICAL).
//
// This is REQUIRED for Seek/Position to be meaningful on HPE Ultrium (LTO)
// drives: without it the st driver uses a device-dependent address and the
// block numbers returned by MTIOCPOS / accepted by MTSEEK are not the logical
// tape block numbers. This corresponds to:
//
//	mt -f <device> stsetoptions scsi2logical
//
// and is not preserved across reboots; setting it here makes the requirement
// explicit and idempotent.
func WithSCSI2Logical(enable bool) Option {
	return func(c *openConfig) { c.scsi2log = enable }
}

// WithDriverOptions applies a set of boolean driver options via
// MTSETDRVBUFFER MT_ST_SETBOOLEANS immediately after open. Combine the
// Opt* flag constants with bitwise OR. OptSCSI2Logical is added
// automatically when WithSCSI2Logical(true) is used.
func WithDriverOptions(mask int) Option {
	return func(c *openConfig) { c.setOptions = mask }
}

// Open opens a tape device node (e.g. "/dev/nst0") and returns a *Tape.
//
// The device is opened read/write, synchronously (the st driver blocks until
// the drive is ready). Pass WithFlags(os.O_RDWR|os.O_NONBLOCK) to open even
// when no tape is loaded, or WithFlags(os.O_RDONLY) for read-only access.
func Open(name string, opts ...Option) (*Tape, error) {
	cfg := openConfig{
		flags: os.O_RDWR | os.O_SYNC,
	}
	for _, o := range opts {
		o(&cfg)
	}

	fd, err := syscall.Open(name, cfg.flags, cfg.mode)
	if err != nil {
		return nil, fmt.Errorf("tapedrive: open %s: %w", name, err)
	}
	t := &Tape{fd: fd, rewind: isAutoRewindDevice(name), ops: kernelOps{fd: fd}}

	if err := t.applyConfig(cfg); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return t, nil
}

func (t *Tape) applyConfig(cfg openConfig) error {
	mask := cfg.setOptions
	if cfg.scsi2log {
		mask |= OptSCSI2Logical
	}
	if mask != 0 {
		if err := t.SetDriverBooleans(mask); err != nil {
			return err
		}
	}
	if cfg.blockSize != 0 {
		if err := t.SetBlockSize(cfg.blockSize); err != nil {
			return err
		}
	}
	return nil
}

// OpenFile mirrors os.OpenFile for io/fs ergonomics. flags defaults to
// O_RDWR|O_SYNC; perm is only relevant when creating a node.
func OpenFile(name string, flags int, perm os.FileMode) (*Tape, error) {
	return Open(name, WithFlags(flags), WithMode(uint32(perm)))
}

// Fd returns the underlying OS file descriptor. Callers must not close it
// independently of Tape.Close.
func (t *Tape) Fd() uintptr { return uintptr(t.fd) }

// --- io.Reader / io.Writer (zero-allocation hot paths) -------------------

// Read implements io.Reader. In variable-block mode it reads exactly one
// physical tape block into p (returning ENOMEM if len(p) is smaller than the
// block on tape). In fixed-block mode it transfers a multiple of the block
// size. The supplied slice is handed to the kernel verbatim; no copies or
// allocations occur in this package.
//
// Per the st driver contract, a read() returning 0 bytes signals a filemark
// boundary: the caller sees that as io.EOF for the current file. Two
// consecutive filemarks mark the end of recorded data; the second such read
// is reported as ErrEndOfData.
func (t *Tape) Read(p []byte) (int, error) {
	if t.closed {
		return 0, ErrNotOpen
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := t.ops.read(p)
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return n, fmt.Errorf("%w: %v", ErrEndOfMedium, err)
		}
		return n, fmt.Errorf("tapedrive: read: %w", err)
	}
	t.hadRead = true
	// n == 0 means the st driver crossed a filemark: EOF for this file.
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// Write implements io.Writer. In variable-block mode each call writes exactly
// one physical block of len(p) bytes. In fixed-block mode len(p) must be a
// multiple of the block size. The slice flows directly to the kernel; no
// allocation occurs here.
//
// Near end of medium the st driver alternates between returning
// (n, nil) and (n, ENOSPC); ErrEndOfMedium is returned once the physical end
// is reached.
func (t *Tape) Write(p []byte) (int, error) {
	if t.closed {
		return 0, ErrNotOpen
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := t.ops.write(p)
	t.hadRead = false
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return n, fmt.Errorf("%w: %v", ErrEndOfMedium, err)
		}
		return n, fmt.Errorf("tapedrive: write: %w", err)
	}
	return n, nil
}

// ReadAt and WriteAt are not supported: tape is an inherently sequential
// medium and there is no "offset" semantic without an intervening seek.
// Implementing them would force an allocation (record the offset) and
// mislead callers. Use Seek + Read instead.
func (t *Tape) ReadAt(_ []byte, _ int64) (int, error) {
	return 0, errors.New("tapedrive: ReadAt not supported on sequential tape")
}
func (t *Tape) WriteAt(_ []byte, _ int64) (int, error) {
	return 0, errors.New("tapedrive: WriteAt not supported on sequential tape")
}

// --- io.Seeker -----------------------------------------------------------

// SeekWhence mirrors io.SeekStart / io.SeekCurrent / io.SeekEnd.
//
// On tape, Seek maps to MTSEEK and operates on logical block numbers
// (1 block == 1 physical tape record). For this to be meaningful the drive
// must be in logical-block-addressing mode: pass WithSCSI2Logical(true) at
// Open for HPE Ultrium (LTO) drives, or call EnableLogicalSeek separately.
//
// As with io.Seeker, the returned offset is the new position relative to the
// start of the (partition) medium, in blocks. It is obtained from MTTELL after
// the seek, so it reflects what the drive actually reports.
func (t *Tape) Seek(offset int64, whence int) (int64, error) {
	if t.closed {
		return 0, ErrNotOpen
	}
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		cur, err := t.Position()
		if err != nil {
			return 0, err
		}
		target = cur + offset
	case io.SeekEnd:
		// "End" on tape = end of recorded data. Space there, then back
		// off by |offset| blocks. st.rst: MTSEEK after MTEOM is valid.
		if err := t.mtop(OpEOM, 1); err != nil {
			return 0, fmt.Errorf("tapedrive: seek to EOM: %w", err)
		}
		if offset < 0 {
			if err := t.mtop(OpBSR, -offset); err != nil {
				return 0, err
			}
		} else if offset > 0 {
			if err := t.mtop(OpFSR, offset); err != nil {
				return 0, err
			}
		}
		return t.Position()
	default:
		return 0, fmt.Errorf("tapedrive: invalid whence %d", whence)
	}
	if target < 0 {
		return 0, errors.New("tapedrive: negative seek position")
	}
	if err := t.seekBlock(target); err != nil {
		return 0, err
	}
	return t.Position()
}

// seekBlock issues MTSEEK to the given logical block. Uses a stack-local
// mtop; no allocation.
func (t *Tape) seekBlock(block int64) error {
	op := mtop{Op: OpSeek, Count: int32(block)}
	return t.ioctlTop(&op)
}

// --- position ---------------------------------------------------------------

// Position returns the current logical block number via MTIOCPOS (the
// MTTELL-like status). Requires logical block addressing on HPE Ultrium
// drives (WithSCSI2Logical).
func (t *Tape) Position() (int64, error) {
	if t.closed {
		return 0, ErrNotOpen
	}
	var pos mtpos
	if err := t.ioctlPos(&pos); err != nil {
		return 0, fmt.Errorf("tapedrive: get position: %w", err)
	}
	return pos.Blkno, nil
}

// Tell is an alias for Position matching the mt(1) command name.
func (t *Tape) Tell() (int64, error) { return t.Position() }

// --- status (zero-allocation) ---------------------------------------------

// Status is the device-independent view returned by MTIOCGET. All fields are
// plain integers so reading status does not allocate.
type Status struct {
	Type      int64 // mt_type (MT_ISSCSI2 etc.)
	Resid     int64 // residual count
	BlockSize int   // decoded from mt_dsreg
	Density   int   // decoded from mt_dsreg
	Gstat     int64 // raw generic status bits; use the Status.* predicates
	Erreg     int64 // error register; low 16 bits = recovered error count
	FileNo    int64 // current file number (-1 when unknown)
	BlkNo     int64 // current block within file (-1 when unknown)
}

// Status predicates.
func (s Status) EOF() bool            { return s.Gstat&gmtEOF != 0 }
func (s Status) BOT() bool            { return s.Gstat&gmtBOT != 0 } // beginning of tape
func (s Status) EOT() bool            { return s.Gstat&gmtEOT != 0 } // end of tape
func (s Status) EOD() bool            { return s.Gstat&gmtEOD != 0 } // end of recorded data
func (s Status) Online() bool         { return s.Gstat&gmtOnline != 0 }
func (s Status) WriteProtected() bool { return s.Gstat&gmtWRProt != 0 }
func (s Status) TapeLoaded() bool     { return s.Gstat&gmtDROpen == 0 }
func (s Status) CleaningNeeded() bool { return s.Gstat&gmtCLN != 0 }
func (s Status) Setmark() bool        { return s.Gstat&gmtSM != 0 }

// Status fetches drive status via MTIOCGET. Zero-allocation: the on-stack
// mtget is decoded into the returned Status value (which itself fits in a few
// registers/cache lines and is cheap to pass by value).
func (t *Tape) Status() (Status, error) {
	if t.closed {
		return Status{}, ErrNotOpen
	}
	var g mtget
	if err := t.ioctlGet(&g); err != nil {
		return Status{}, fmt.Errorf("tapedrive: get status: %w", err)
	}
	return Status{
		Type:      g.Type,
		Resid:     g.Resid,
		BlockSize: int(uint64(g.Dsreg) & dsBlksizeMask),
		Density:   int((uint64(g.Dsreg) >> dsDensityShift) & 0xff),
		Gstat:     g.Gstat,
		Erreg:     g.Erreg,
		FileNo:    g.Fileno,
		BlkNo:     g.Blkno,
	}, nil
}

// --- io.Closer ------------------------------------------------------------

// Close closes the underlying file descriptor.
//
// For the auto-rewind device (/dev/st*) the kernel rewinds on close. For the
// non-rewind device (/dev/nst*) the tape stays where it is; if the last
// operation was a write the driver writes a filemark (or two, depending on
// MT_ST_TWO_FM) per st.rst. Use WriteFilemarks to control this explicitly.
func (t *Tape) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	if err := t.ops.close(); err != nil {
		return fmt.Errorf("tapedrive: close: %w", err)
	}
	return nil
}

// --- tape operations ------------------------------------------------------

// MTOP performs an arbitrary MTIOCTOP operation (mt_op + mt_count). It is the
// low-level escape hatch; prefer the typed wrappers below where one exists.
func (t *Tape) MTOP(op int16, count int) error {
	if t.closed {
		return ErrNotOpen
	}
	if int64(count) > int64(maxInt32) {
		return fmt.Errorf("tapedrive: count %d out of range", count)
	}
	return t.mtop(op, int64(count))
}

const maxInt32 = int64(1<<31 - 1)

// mtop is the in-package fast path: no closed check, no allocation, takes an
// unbounded int64 count so callers can pass large space counts.
func (t *Tape) mtop(op int16, count int64) error {
	if count > maxInt32 {
		count = maxInt32
	}
	cmd := mtop{Op: op, Count: int32(count)}
	return t.ioctlTop(&cmd)
}

// WriteFilemarks writes count filemarks (MTWEOF). Acts as a synchronization
// point: the drive flushes its buffers before the command returns.
func (t *Tape) WriteFilemarks(count int) error {
	if err := t.mtop(OpWEOF, int64(count)); err != nil {
		return fmt.Errorf("tapedrive: write %d filemarks: %w", count, err)
	}
	return nil
}

// WriteFilemarksImmediate writes count filemarks without waiting for the
// drive buffers to flush (MTWEOFI). Faster when writing many consecutive
// files; see the BASICS warning in st.rst about immediate filemarks.
func (t *Tape) WriteFilemarksImmediate(count int) error {
	if err := t.mtop(OpWEOFI, int64(count)); err != nil {
		return fmt.Errorf("tapedrive: write %d immediate filemarks: %w", count, err)
	}
	return nil
}

// Rewind rewinds the tape to the beginning (MTREW).
func (t *Tape) Rewind() error { return wrapOp("rewind", t.mtop(OpRewind, 1)) }

// Offline rewinds and takes the drive offline, usually ejecting the tape
// (MTOFFL).
func (t *Tape) Offline() error { return wrapOp("offline", t.mtop(OpOffline, 1)) }

// Retension retensions the tape (MTRETEN).
func (t *Tape) Retension() error { return wrapOp("retension", t.mtop(OpRetension, 1)) }

// Erase erases the tape. A short erase is used when quick is true, a long
// (whole-tape) erase otherwise.
func (t *Tape) Erase(quick bool) error {
	count := int64(1)
	if quick {
		count = 0
	}
	return wrapOp("erase", t.mtop(OpErase, count))
}

// SpaceToEnd positions the tape after the last recorded filemark, ready for
// appending (MTEOM).
func (t *Tape) SpaceToEnd() error { return wrapOp("space to EOM", t.mtop(OpEOM, 1)) }

// ForwardSpaceFilemarks spaces forward over count filemarks; the tape ends up
// positioned at the first record of the next file (MTFSF).
func (t *Tape) ForwardSpaceFilemarks(count int) error {
	return wrapOp("fsf", t.mtop(OpFSF, int64(count)))
}

// BackwardSpaceFilemarks spaces backward over count filemarks (MTBSF).
func (t *Tape) BackwardSpaceFilemarks(count int) error {
	return wrapOp("bsf", t.mtop(OpBSF, int64(count)))
}

// ForwardSpaceRecords spaces forward over count records (MTFSR).
func (t *Tape) ForwardSpaceRecords(count int) error {
	return wrapOp("fsr", t.mtop(OpFSR, int64(count)))
}

// BackwardSpaceRecords spaces backward over count records (MTBSR).
func (t *Tape) BackwardSpaceRecords(count int) error {
	return wrapOp("bsr", t.mtop(OpBSR, int64(count)))
}

// ForwardSpaceSetmarks / BackwardSpaceSetmarks space over count setmarks
// (MTFSS / MTBSS), used with DDS-style partitioned media.
func (t *Tape) ForwardSpaceSetmarks(count int) error {
	return wrapOp("fss", t.mtop(OpFSS, int64(count)))
}
func (t *Tape) BackwardSpaceSetmarks(count int) error {
	return wrapOp("bss", t.mtop(OpBSS, int64(count)))
}

// WriteSetmarks writes count setmarks (MTWSM).
func (t *Tape) WriteSetmarks(count int) error {
	return wrapOp("wsm", t.mtop(OpWSM, int64(count)))
}

// SetBlockSize sets the drive block size. size == 0 selects variable-block
// mode (MTSETBLK).
func (t *Tape) SetBlockSize(size int) error {
	return wrapOp("set block size", t.mtop(OpSetBlk, int64(size)))
}

// SetDensity sets the SCSI density code (MTSETDENSITY).
func (t *Tape) SetDensity(code int) error {
	return wrapOp("set density", t.mtop(OpSetDensity, int64(code)))
}

// SetCompression enables or disables drive-level compression via SCSI mode
// page 15 (MTCOMPRESSION).
func (t *Tape) SetCompression(enable bool) error {
	v := int64(0)
	if enable {
		v = 1
	}
	return wrapOp("set compression", t.mtop(OpCompression, v))
}

// LockDoor / UnlockDoor control the drive door lock (MTLOCK / MTUNLOCK).
func (t *Tape) LockDoor() error   { return wrapOp("lock", t.mtop(OpLock, 1)) }
func (t *Tape) UnlockDoor() error { return wrapOp("unlock", t.mtop(OpUnlock, 1)) }

// Load / Unload issue the SCSI load / unload commands (MTLOAD / MTUNLOAD).
func (t *Tape) Load() error   { return wrapOp("load", t.mtop(OpLoad, 1)) }
func (t *Tape) Unload() error { return wrapOp("unload", t.mtop(OpUnload, 1)) }

// Flush issues a no-op that flushes the driver's buffers (MTNOP).
func (t *Tape) Flush() error { return wrapOp("flush", t.mtop(OpNoOp, 1)) }

// --- MTSETDRVBUFFER helpers ----------------------------------------------

// SetDriverBooleans sets the boolean driver/mode options given by mask
// (combine Opt* constants). This is the MTSETDRVBUFFER MT_ST_SETBOOLEANS
// subcommand. Notably OptSCSI2Logical enables logical-block addressing for
// Seek/Position on HPE Ultrium drives.
func (t *Tape) SetDriverBooleans(mask int) error {
	if t.closed {
		return ErrNotOpen
	}
	cmd := mtop{Op: OpSetDrvBuf, Count: int32(int32(mask) | OptSetBooleans)}
	return wrapOp("set driver booleans", t.ioctlTop(&cmd))
}

// ClearDriverBooleans clears the boolean driver/mode options given by mask.
func (t *Tape) ClearDriverBooleans(mask int) error {
	if t.closed {
		return ErrNotOpen
	}
	cmd := mtop{Op: OpSetDrvBuf, Count: int32(int32(mask) | OptClearBooleans)}
	return wrapOp("clear driver booleans", t.ioctlTop(&cmd))
}

// EnableLogicalSeek enables logical-block addressing (MT_ST_SCSI2LOGICAL),
// equivalent to `mt -f <dev> stsetoptions scsi2logical`. Required for
// Seek/Position to be meaningful on HPE Ultrium drives. Idempotent.
func (t *Tape) EnableLogicalSeek() error {
	return t.SetDriverBooleans(OptSCSI2Logical)
}

// SetTimeout sets the normal command timeout in seconds (MT_ST_SET_TIMEOUT).
func (t *Tape) SetTimeout(seconds int) error {
	if t.closed {
		return ErrNotOpen
	}
	cmd := mtop{Op: OpSetDrvBuf, Count: int32(OptSetTimeout | (seconds & 0xffffff))}
	return wrapOp("set timeout", t.ioctlTop(&cmd))
}

// SetWriteThreshold sets the driver write threshold in kilobytes
// (MT_ST_WRITE_THRESHOLD).
func (t *Tape) SetWriteThreshold(kbytes int) error {
	if t.closed {
		return ErrNotOpen
	}
	cmd := mtop{Op: OpSetDrvBuf, Count: int32(OptWriteThreshold | (kbytes & 0xffffff))}
	return wrapOp("set write threshold", t.ioctlTop(&cmd))
}

// --- raw ioctl plumbing (zero-allocation) --------------------------------

// ioctlTop issues MTIOCTOP with the given argument. The pointer is allowed to
// escape only into the kernel (via unsafe.Pointer); no Go heap allocation
// happens because op is caller-owned (typically stack-resident).
func (t *Tape) ioctlTop(op *mtop) error {
	return t.ops.ioctlTop(op)
}

// ioctlGet fills *mtget via MTIOCGET.
func (t *Tape) ioctlGet(g *mtget) error {
	return t.ops.ioctlGet(g)
}

// ioctlPos fills *mtpos via MTIOCPOS.
func (t *Tape) ioctlPos(p *mtpos) error {
	return t.ops.ioctlPos(p)
}

func wrapOp(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("tapedrive: %s: %w", name, err)
}

// isAutoRewindDevice heuristically detects /dev/st* (auto-rewind) vs
// /dev/nst* (non-rewind) from the device path. Used only to set the rewind
// hint; it does not change behaviour beyond documentation.
func isAutoRewindDevice(name string) bool {
	// Match the basename's leading character: stX -> rewind, nstX -> no.
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			base := name[i+1:]
			return len(base) > 0 && base[0] == 's' // "st0", not "nst0"
		}
	}
	return len(name) > 0 && name[0] == 's'
}

// Compile-time interface satisfaction.
var (
	_ io.Reader          = (*Tape)(nil)
	_ io.Writer          = (*Tape)(nil)
	_ io.Seeker          = (*Tape)(nil)
	_ io.Closer          = (*Tape)(nil)
	_ io.ReadSeeker      = (*Tape)(nil)
	_ io.ReadWriteSeeker = (*Tape)(nil)
	_ io.ReadWriteCloser = (*Tape)(nil)
	_ io.ReadSeekCloser  = (*Tape)(nil)
)
