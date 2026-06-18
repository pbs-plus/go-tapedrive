// Package tapedrive provides Go io-compatible interfaces to the Linux SCSI
// tape (st) driver.
//
// A Tape implements io.Reader, io.Writer, io.Seeker, io.Closer (and therefore
// io.ReadSeeker, io.ReadWriteSeeker, io.ReadWriteCloser, io.ReadSeekCloser,
// io.ReadSeekCloser) over a /dev/st* or /dev/nst* device node.
//
// The Read/Write hot paths perform no heap allocations: all ioctl argument
// structures and syscall registers are kept on the stack or inside the Tape
// value, and byte slices flow directly between the caller and the kernel.
//
// Seek maps onto MTSEEK and Position/Tell onto MTIOCPOS, both in logical
// block numbers. HPE Ultrium (LTO) drives require the MT_ST_SCSI2LOGICAL
// option (see WithSCSI2Logical / EnableLogicalSeek) for these to be
// meaningful; otherwise the driver uses a device-dependent address.
//
// Read follows st.rst: a single zero-byte read at a filemark is reported as
// io.EOF (end of the current file); two consecutive zero-byte reads are
// reported as [ErrEndOfData]. Any tape-movement op resets the filemark
// counter. Near end of medium the first write ENOSPC is [ErrEarlyWarning]
// (one trailer write still permitted); a subsequent ENOSPC is
// [ErrEndOfMedium].
//
// The struct mtget wire layout matches the kernel/glibc UAPI: mt_fileno and
// mt_blkno are __daddr_t (4-byte int), so sizeof == 48 and the MTIOCGET
// request number is 0x80306d02 on the supported architectures.
//
// References:
//   - Documentation/scsi/st.rst (Linux kernel)
//   - man 4 st
//   - include/uapi/linux/mtio.h
package tapedrive
