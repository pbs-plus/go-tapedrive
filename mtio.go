package tapedrive

import "unsafe"

// MTIO operations (mt_op). Values match include/uapi/linux/mtio.h.
const (
	OpReset       int16 = 0  // reset drive
	OpFSF         int16 = 1  // forward space over count filemarks
	OpBSF         int16 = 2  // backward space over count filemarks
	OpFSR         int16 = 3  // forward space over count records
	OpBSR         int16 = 4  // backward space over count records
	OpWEOF        int16 = 5  // write count filemarks
	OpRewind      int16 = 6  // rewind
	OpOffline     int16 = 7  // rewind and put drive offline (eject)
	OpNoOp        int16 = 8  // no-op, flush buffers / refresh status
	OpRetension   int16 = 9  // retension tape
	OpBSFM        int16 = 10 // backward space over count filemarks, position at FM
	OpFSFM        int16 = 11 // forward space over count filemarks, position at FM
	OpEOM         int16 = 12 // space to end of recorded data
	OpErase       int16 = 13 // erase tape
	OpSetBlk      int16 = 20 // set block length (0 = variable)
	OpSetDensity  int16 = 21 // set density code
	OpSeek        int16 = 22 // seek to block (QFA / SCSI-2 logical)
	OpTell        int16 = 23 // tell block position
	OpSetDrvBuf   int16 = 24 // set drive buffering / driver options
	OpFSS         int16 = 25 // space forward over count setmarks
	OpBSS         int16 = 26 // space backward over count setmarks
	OpWSM         int16 = 27 // write count setmarks
	OpLock        int16 = 28 // lock drive door
	OpUnlock      int16 = 29 // unlock drive door
	OpLoad        int16 = 30 // SCSI load
	OpUnload      int16 = 31 // SCSI unload
	OpCompression int16 = 32 // enable/disable compression (mode page 15)
	OpSetPart     int16 = 33 // change active partition
	OpMkPart      int16 = 34 // format tape with one or two partitions
	OpWEOFI       int16 = 35 // write count filemarks in immediate mode
)

// MTSETDRVBUFFER subcommand masks (apply to mt_count with MT_ST_OPTIONS).
const (
	OptOptions        = 0xf0000000
	OptBooleans       = 0x10000000
	OptSetBooleans    = 0x30000000
	OptClearBooleans  = 0x40000000
	OptWriteThreshold = 0x20000000
	OptDefBlksize     = 0x50000000
	OptDefOptions     = 0x60000000
	OptTimeouts       = 0x70000000
	OptSetTimeout     = OptTimeouts
	OptSetLongTimeout = OptTimeouts | 0x100000
	OptSetCln         = 0x80000000
	OptDefDensity     = OptDefOptions | 0x100000
	OptDefCompression = OptDefOptions | 0x200000
	OptDefDrvBuffer   = OptDefOptions | 0x300000
	OptClearDefault   = 0xfffff
)

// Boolean driver/mode option bits usable with MTSETDRVBUFFER.
const (
	OptBufferWrites  = 0x1
	OptAsyncWrites   = 0x2
	OptReadAhead     = 0x4
	OptDebugging     = 0x8
	OptTwoFM         = 0x10
	OptFastMTEOM     = 0x20
	OptAutoLock      = 0x40
	OptDefWrites     = 0x80
	OptCanBSR        = 0x100
	OptNoBlkLims     = 0x200
	OptCanPartitions = 0x400
	OptSCSI2Logical  = 0x800 // required for logical-block Seek/Position on HPE Ultrium
	OptSysV          = 0x1000
	OptNoWait        = 0x2000
	OptSILI          = 0x4000
	OptNoWaitEOF     = 0x8000
)

// Status register (mt_dsreg) subfield shifts/masks.
const (
	dsBlksizeShift = 0
	dsBlksizeMask  = 0xffffff
	dsDensityShift = 24
	dsDensityMask  = 0xff000000
	dsSofterrMask  = 0xffff
)

// Generic status (mt_gstat) bit masks.
const (
	gmtEOF     = 0x80000000
	gmtBOT     = 0x40000000
	gmtEOT     = 0x20000000
	gmtSM      = 0x10000000 // DDS setmark
	gmtEOD     = 0x08000000 // DDS EOD
	gmtWRProt  = 0x04000000
	gmtOnline  = 0x01000000
	gmtDROpen  = 0x00040000 // door open (no tape)
	gmtIMRepEn = 0x00010000 // immediate report mode
	gmtCLN     = 0x00008000 // cleaning requested
)

// mtop is the C "struct mtop" { short mt_op; int mt_count; } from mtio.h.
// sizeof == 8 on amd64/arm64 (2 bytes padding between mt_op and mt_count).
type mtop struct {
	Op    int16
	_     [2]byte
	Count int32
}

// mtget is "struct mtget" from mtio.h. Field order matches the kernel UAPI so
// the ioctl copies the right bytes into each field.
type mtget struct {
	Type   int64 // mt_type
	Resid  int64 // mt_resid
	Dsreg  int64 // mt_dsreg
	Gstat  int64 // mt_gstat
	Erreg  int64 // mt_erreg
	Fileno int64 // mt_fileno
	Blkno  int64 // mt_blkno
}

// mtpos is "struct mtpos" { long mt_blkno; } from mtio.h.
type mtpos struct {
	Blkno int64
}

const (
	sizeofMtop  = int(unsafe.Sizeof(mtop{}))
	sizeofMtget = int(unsafe.Sizeof(mtget{}))
	sizeofMtpos = int(unsafe.Sizeof(mtpos{}))
)

// ioctl request numbers, computed via the asm-generic _IOC macros so they are
// correct on any Linux arch without arch-specific constants.
var (
	ioctlMTIOCTOP = makeIOC(iocWriteDir, 'm', 1, sizeofMtop)
	ioctlMTIOCGET = makeIOC(iocReadDir, 'm', 2, sizeofMtget)
	ioctlMTIOCPOS = makeIOC(iocReadDir, 'm', 3, sizeofMtpos)
)

const (
	iocNrbits   = 8
	iocTypebits = 8
	iocSizebits = 14
	iocDirbits  = 2

	iocNrShift   = 0
	iocTypeShift = iocNrShift + iocNrbits
	iocSizeShift = iocTypeShift + iocTypebits
	iocDirShift  = iocSizeShift + iocSizebits

	iocNoneDir  = 0
	iocWriteDir = 1
	iocReadDir  = 2
)

func ioc(dir int, typ byte, nr, size int) uintptr {
	return uintptr(
		(dir << iocDirShift) |
			(int(typ) << iocTypeShift) |
			(size << iocSizeShift) |
			(nr << iocNrShift),
	)
}

func makeIOC(dir int, typ byte, nr, size int) uintptr { return ioc(dir, typ, nr, size) }
