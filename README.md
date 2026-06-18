# go-tapedrive

A Go 1.26 library that exposes the Linux SCSI tape (`st`) driver through the
standard `io` interfaces — `io.Reader`, `io.Writer`, `io.Seeker`, `io.Closer`
and every composition thereof (`io.ReadSeeker`, `io.ReadWriteSeeker`,
`io.ReadWriteCloser`, `io.ReadSeekCloser`).

The Read / Write / Seek / Status hot paths perform **zero heap allocations**:
the kernel copies directly into and out of caller-supplied buffers and every
ioctl argument lives on the stack or inside the `Tape` struct.

## Install

```go
import "github.com/pbs-plus/go-tapedrive"
```

`go.mod` declares `go 1.26`. No external dependencies — only the standard
library (`syscall`, `unsafe`).

## Usage

```go
t, err := tapedrive.Open("/dev/nst0",
    tapedrive.WithBlockSize(0),            // variable-block mode
    tapedrive.WithSCSI2Logical(true),      // required for Seek/Position on HPE Ultrium
)
if err != nil { log.Fatal(err) }
defer t.Close()

// Write a record (one physical tape block per Write in variable mode).
if _, err := t.Write(rec); err != nil { log.Fatal(err) }

// Filemark, rewind, read back.
if err := t.WriteFilemarks(1); err != nil { log.Fatal(err) }
if err := t.Rewind(); err != nil { log.Fatal(err) }

buf := make([]byte, maxBlock)
for {
    n, err := t.Read(buf)          // io.EOF = filemark boundary
    if err == io.EOF { break }
    if err != nil { log.Fatal(err) }
    consume(buf[:n])
}
```

It slots directly into `io.Copy`, `bufio`, `archive/tar`, etc.

## HPE Ultrium (LTO) drives — seek & tell

The `mt` seek/tell commands need logical block addressing. With the `st`
driver that means `MT_ST_SCSI2LOGICAL`:

```sh
mt -f /dev/nst0 stsetoptions scsi2logical
```

That setting is **not preserved across reboots**. The library makes it
explicit and idempotent instead:

```go
tapedrive.Open("/dev/nst0", tapedrive.WithSCSI2Logical(true))
// or at runtime:
t.EnableLogicalSeek()
```

`Seek` then maps to `MTSEEK` and `Position`/`Tell` to `MTIOCPOS`, both in
logical block numbers.

## API surface

`Open`, `OpenFile`, `Fd`, plus the `io` methods. Tape-specific operations:

`Rewind`, `Offline`, `Retension`, `Erase`, `SpaceToEnd`,
`ForwardSpaceFilemarks`, `BackwardSpaceFilemarks`,
`ForwardSpaceRecords`, `BackwardSpaceRecords`,
`ForwardSpaceSetmarks`, `BackwardSpaceSetmarks`,
`WriteFilemarks`, `WriteFilemarksImmediate`, `WriteSetmarks`,
`SetBlockSize`, `SetDensity`, `SetCompression`,
`LockDoor`, `UnlockDoor`, `Load`, `Unload`, `Flush`,
`Seek`, `Position`/`Tell`, `Status`,
`SetDriverBooleans`, `ClearDriverBooleans`, `EnableLogicalSeek`,
`SetTimeout`, `SetWriteThreshold`, and a low-level `MTOP`.

## Error handling

Sentinels: `ErrEndOfData`, `ErrEndOfMedium`, `ErrNoTape`,
`ErrWriteProtected`, `ErrCleanRequested`, `ErrNotOpen`. Match with
`errors.Is`. `Read` returns `io.EOF` at a filemark boundary so it composes
cleanly with the rest of `io`.

## Platform

Linux only (the `st` driver). The ioctl request numbers are computed at
package init from the asm-generic `_IOC` macros, so the package is correct on
every Linux architecture without arch-specific constants.

## References

* `Documentation/scsi/st.rst` — the Linux kernel SCSI tape driver docs.
* `include/uapi/linux/mtio.h` — `struct mtop`, `struct mtget`, `struct mtpos`,
  the `MT*` operations and `MT_ST_*` option bits.
