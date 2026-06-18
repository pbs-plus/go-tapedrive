//go:build linux

package tapedrive

// Raw Linux SCSI tape (st) constants, structures, and ioctl helpers, taken
// from <linux/mtio.h> and <asm-generic/ioctl.h>. This is the wire-level layer;
// the block-oriented API lives in tapedrive.go.

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ErrEndOfData is returned by ReadBlock when the drive signals end of recorded
// data (two consecutive zero-length reads, per st(4)). It is distinct from
// io.EOF, which means a single filemark was crossed (more data may follow in
// the next file).
var ErrEndOfData = errors.New("tapedrive: end of recorded data")

// ErrShortBuffer is returned by ReadBlockInto when the supplied buffer is too
// small for the next tape record (variable-block mode). Retry with a larger
// buffer, or use ReadBlock which auto-sizes.
var ErrShortBuffer = errors.New("tapedrive: buffer too small for record")

// ioctl requests, computed from the _IOW/_IOR macros so the embedded
// struct-size field always matches the actual Go struct (a wrong size field is
// rejected by the kernel with EINVAL).
var (
	mtioctop = iow('m', 1, unsafe.Sizeof(mtop{}))  // MTIOCTOP  _IOW('m',1,struct mtop)
	mtioCget = ior('m', 2, unsafe.Sizeof(mtget{})) // MTIOCGET  _IOR('m',2,struct mtget)
	mtioCpos = ior('m', 3, unsafe.Sizeof(mtpos{})) // MTIOCPOS  _IOR('m',3,struct mtpos)
)

// Magnetic tape operations (MTIOCTOP op codes; see st(4)).
const (
	mtfsf  = 1  // forward space over filemark
	mtbsf  = 2  // backward space over filemark
	mtfsr  = 3  // forward space record
	mtbsr  = 4  // backward space record
	mtweof = 5  // write filemark
	mtrew  = 6  // rewind
	mtnop  = 8  // no op (flush + set status)
	mteom  = 12 // go to end of recorded media
	mtseek = 22 // seek to block number (SCSI LOCATE)
)

// mtop is the argument to MTIOCTOP.
type mtop struct {
	Op    int16
	Count int32
}

// mtget is the argument to MTIOCGET. Layout must match <linux/mtio.h>. On
// x86-64, mt_fileno/mt_blkno are __kernel_daddr_t (int32), so the struct is
// 48 bytes — NOT 56. A mismatch makes the kernel reject MTIOCGET with EINVAL.
type mtget struct {
	Type   int64
	Resid  int64
	Dsreg  int64
	Gstat  int64
	Erreg  int64
	Fileno int32 // __kernel_daddr_t on x86-64
	Blkno  int32 // __kernel_daddr_t on x86-64
}

// mtpos is the argument to MTIOCPOS.
type mtpos struct {
	Blkno int64
}

// Status-bit masks for mtget.Gstat (GMT_* macros in mtio.h).
const (
	GMTEOF    = 0x80000000 // positioned just after a filemark
	GMTBOT    = 0x40000000 // at beginning of tape
	GMTEOT    = 0x20000000 // a tape op reached physical end of tape
	GMTEOD    = 0x08000000 // at end of recorded data
	GMTWRProt = 0x04000000 // write-protected
	GMTOnline = 0x01000000 // tape loaded and ready
)

const (
	dsregBlksizeShift = 0
	dsregBlksizeMask  = 0xffffff
)

// ioctl performs a raw tape ioctl on fd.
func ioctl(fd int, req uint, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), arg)
	if errno != 0 {
		return errno
	}
	return nil
}

// _IOC encoding from <asm-generic/ioctl.h> (DIRBITS=2, SIZEBITS=14):
//
//	(dir<<30) | (size<<16) | (type<<8) | nr
//
// _IOC_NONE=0, _IOC_WRITE=1 (user->kernel), _IOC_READ=2 (kernel->user).
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ byte, nr, size uintptr) uint {
	return uint(dir)<<30 | uint(size)<<16 | uint(typ)<<8 | uint(nr)&0xff
}
func iow(typ byte, nr, size uintptr) uint { return ioc(iocWrite, typ, nr, size) }
func ior(typ byte, nr, size uintptr) uint { return ioc(iocRead, typ, nr, size) }
