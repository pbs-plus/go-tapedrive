//go:build linux

package tapedrive

// Raw Linux SCSI tape (st) constants and structures, taken verbatim from
// <linux/mtio.h>. This file holds the wire-level layer; the byte-oriented
// Reader/Seeker API lives in tapedrive.go.

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl requests, computed from the _IOW/_IOR macros in <asm-generic/ioctl.h>
// so the embedded struct-size field is always correct (a wrong size field is
// rejected by the kernel with EINVAL).
var (
	mtioctop = iow('m', 1, unsafe.Sizeof(mtop{}))  // MTIOCTOP  _IOW('m',1,struct mtop)
	mtioCget = ior('m', 2, unsafe.Sizeof(mtget{})) // MTIOCGET  _IOR('m',2,struct mtget)
)

// Magnetic tape operations (subset; see MTIOCTOP in st(4)).
const (
	mtbsf    = 2  // backward space over filemark
	mtfsr    = 3  // forward space record
	mtbsr    = 4  // backward space record
	mtweof   = 5  // write filemark
	mtrew    = 6  // rewind
	mtnop    = 8  // no op (flush + set status)
	mtbsfm   = 10 // backward space filemark, position at FM
	mtfsfm   = 11 // forward space filemark, position at FM
	mteom    = 12 // go to end of recorded media
	mtsetblk = 20 // set block length (0 = variable)
	mtseek   = 22 // seek to block number
)

// errnoENOMEM matches the ENOMEM reported by st(4) when a read buffer is too
// small for the next physical block.
const errnoENOMEM = unix.ENOMEM

// mtop is the argument to MTIOCTOP.
type mtop struct {
	Op    int16
	Count int32
}

// mtget is the argument to MTIOCGET. Layout must match <linux/mtio.h> on the
// target arch. On x86-64, __kernel_daddr_t (used for mt_fileno/mt_blkno) is
// 4 bytes (int), so the struct is 48 bytes — NOT 56. Getting this wrong makes
// the kernel reject MTIOCGET with EINVAL because the size field embedded in
// the ioctl number won't match.
type mtget struct {
	Type   int64
	Resid  int64
	Dsreg  int64
	Gstat  int64
	Erreg  int64
	Fileno int32 // __kernel_daddr_t on x86-64
	Blkno  int32 // __kernel_daddr_t on x86-64
}

const (
	dsregBlksizeShift = 0
	dsregBlksizeMask  = 0xffffff
)

// Status-bit masks for mtget.Gstat (GMT_* macros in mtio.h), exposed for
// callers that want to interpret Status().
const (
	GMTEOF    = 0x80000000
	GMTBOT    = 0x40000000
	GMTEOT    = 0x20000000
	GMTEOD    = 0x08000000
	GMTWRProt = 0x04000000
	GMTOnline = 0x01000000
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

// isErrno reports whether err wraps the given errno.
func isErrno(err error, target error) bool { return errors.Is(err, target) }

// fdSource adapts an *os.File to the recordSource interface used by Drive.
// On the st driver, each read(2) returns exactly one record.
type fdSource struct {
	f *os.File
}

func (s *fdSource) readRecord(fetch []byte) (int, error) {
	return s.f.Read(fetch)
}

func (s *fdSource) grow(cap int) []byte {
	next := min(cap*2, MaxReadBuffer)
	return make([]byte, next)
}
