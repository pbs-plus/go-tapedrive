// Package tapedrive is a Go binding for the Linux SCSI tape (st) driver.
//
// A Tape is a handle to a /dev/st* or /dev/nst* device node. Tape is a
// record-oriented medium — a sequence of blocks grouped into files separated
// by filemarks — so Tape deliberately does not implement io.Reader,
// io.Writer or io.Seeker. Those interfaces model a flat byte stream, which a
// tape is not, and forcing them onto it (filemarks as io.EOF, byte offsets
// for Seek, hidden zero-read counters) makes the API misleading. Instead:
//
//   - ReadBlock / WriteBlock move one physical tape block per call.
//   - A filemark seen while reading is reported as [ErrFilemark]; the tape
//     then sits at the first block of the next file. Two filemarks in a row
//     with no data between them are [ErrEndOfData].
//   - SeekBlock takes an absolute logical block number (MTSEEK); Position
//     reads it back (MTIOCPOS). Both need logical-block addressing on HPE
//     Ultrium drives — see WithSCSI2Logical / EnableLogicalSeek.
//
// The ReadBlock / WriteBlock hot paths perform no heap allocations: all ioctl
// argument structures and syscall registers are kept on the stack or inside
// the Tape value, and byte slices flow directly between the caller and the
// kernel.
//
// Near end of medium the first write ENOSPC is [ErrEarlyWarning] (one trailer
// write still permitted); a subsequent ENOSPC is [ErrEndOfMedium].
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
