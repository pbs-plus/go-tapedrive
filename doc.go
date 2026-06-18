// Package tapedrive provides Go io-compatible interfaces to the Linux SCSI
// tape (st) driver.
//
// A Tape implements io.Reader, io.Writer, io.Seeker, io.Closer (and therefore
// io.ReadSeeker, io.ReadWriteSeeker, io.ReadWriteCloser, io.ReadSeekCloser,
// io.ReadWriteSeekCloser) over a /dev/st* or /dev/nst* device node.
//
// The Read/Write hot paths perform no heap allocations: all ioctl argument
// structures and syscall registers are kept on the stack or inside the Tape
// value, and byte slices flow directly between the caller and the kernel.
//
// Seek maps onto MTSEEK (logical block addressing). HPE Ultrium drives require
// the MT_ST_SCSI2LOGICAL option (see Tape.EnableSCSI2Logical / EnableLogicalSeek)
// for Seek/Position to be meaningful, otherwise the driver uses a device
// dependent address. Tell is exposed via Position.
//
// References:
//   - Documentation/scsi/st.rst (Linux kernel)
//   - include/uapi/linux/mtio.h
package tapedrive
