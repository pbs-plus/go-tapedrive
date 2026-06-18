# go-tapedrive

A Go 1.26 binding for the Linux SCSI tape (`st`) driver.

A tape is a **record-oriented** medium — a sequence of blocks grouped into
files separated by filemarks — **not** a byte stream. So `Tape` deliberately
does **not** implement `io.Reader`, `io.Writer` or `io.Seeker`. Forcing those
interfaces onto a tape (filemarks as `io.EOF`, byte offsets for `Seek`, hidden
zero-read counters) makes the API misleading. Instead `ReadBlock` / `WriteBlock`
move one physical tape block per call, and a filemark seen while reading is an
explicit `ErrFilemark`.

The `ReadBlock` / `WriteBlock` / `SeekBlock` / `Status` hot paths perform
**zero heap allocations**: the kernel copies directly into and out of
caller-supplied buffers and every ioctl argument lives on the stack or inside
the `Tape` struct.

## Install

```go
import "github.com/pbs-plus/go-tapedrive"
```

`go.mod` declares `go 1.26`. No external dependencies — only the standard
library (`syscall`, `unsafe`).

## Usage

```go
t, err := tapedrive.Open("/dev/nst0",
    tapedrive.WithBlockSize(0),       // variable-block mode (default)
    tapedrive.WithSCSI2Logical(true), // required for SeekBlock/Position on HPE Ultrium
)
if err != nil { log.Fatal(err) }
defer t.Close()

// Write a record (one physical tape block per WriteBlock in variable mode).
if _, err := t.WriteBlock(rec); err != nil { log.Fatal(err) }

// Filemark, rewind, read back.
if err := t.WriteFilemarks(1); err != nil { log.Fatal(err) }
if err := t.Rewind(); err != nil { log.Fatal(err) }

buf := make([]byte, maxBlock)
for {
    n, err := t.ReadBlock(buf)
    switch {
    case errors.Is(err, tapedrive.ErrEndOfData):
        return // two filemarks in a row: no more recorded data on the tape
    case errors.Is(err, tapedrive.ErrFilemark):
        continue // crossed one filemark: end of the current file
    case err != nil:
        return err
    }
    consume(buf[:n])
}
```

## Read semantics

The `st` driver returns zero bytes at a filemark. This library surfaces that
directly instead of hiding it behind `io.EOF`:

* A **single** zero-byte read is a filemark boundary — reported as
  `ErrFilemark`. The next `ReadBlock` returns the first block of the next
  file.
* **Two consecutive** zero-byte reads (two filemarks with no data between)
  mean end of recorded data — reported as `ErrEndOfData`.
* Any tape-movement op (`SeekBlock`, `Rewind`, `ForwardSpace*`, `Offline`, …)
  resets the consecutive-zero counter, so a single filemark after a reposition
  is always `ErrFilemark`, never a spurious `ErrEndOfData`.

## Write semantics near end of medium

Per `st.rst` ("EOM Behaviour When Writing"), the first `ENOSPC` from the drive
is the **early warning**: the current write still completes and one trailer
write is still permitted.

* The first `ENOSPC` is reported as `ErrEarlyWarning` — the caller may retry
  once to write a trailer.
* A subsequent `ENOSPC` is reported as `ErrEndOfMedium` — the physical end of
  medium has been reached.

## HPE Ultrium (LTO) drives — seek & position

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

`SeekBlock` then maps to `MTSEEK` and `Position` to `MTIOCPOS`, both in
logical block numbers. `SeekBlock` is absolute; to reach the end of recorded
data use `SpaceToEnd`.

## Status

`Status()` decodes the `MTIOCGET` response into plain Go fields:

| Field | Source |
| --- | --- |
| `Type` | `mt_type` (e.g. `MT_ISSCSI2`) |
| `Resid` | `mt_resid` |
| `BlockSize` / `Density` | decoded from `mt_dsreg` |
| `Gstat` | raw `mt_gstat` bits; use the predicates below |
| `SoftErrorCount` | recovered-error count (low 16 bits of `mt_erreg`) |
| `FileNo` / `BlkNo` | `mt_fileno` / `mt_blkno` (`-1` when unknown) |

Predicates: `EOF`, `BOT`, `EOT`, `EOD`, `Online`, `WriteProtected`,
`TapeLoaded`, `CleaningNeeded`, `Setmark`.

## API surface

Lifecycle / records: `Open`, `Close`, `Fd`, `ReadBlock`, `WriteBlock`,
`SeekBlock`, `Position`, `Status`.

Options (functional options on `Open`): `WithFlags`, `WithMode`,
`WithBlockSize`, `WithSCSI2Logical`, `WithDriverOptions`.

Tape movement: `Rewind`, `Offline`, `Retension`, `Erase`, `SpaceToEnd`,
`ForwardSpaceFilemarks`, `BackwardSpaceFilemarks`, `ForwardSpaceRecords`,
`BackwardSpaceRecords`, `ForwardSpaceSetmarks`, `BackwardSpaceSetmarks`,
`Load`, `Unload`, `Flush`.

Writing marks: `WriteFilemarks`, `WriteFilemarksImmediate`, `WriteSetmarks`.

Drive settings: `SetBlockSize`, `SetDensity`, `SetCompression`, `LockDoor`,
`UnlockDoor`, `SetDriverBooleans`, `ClearDriverBooleans`, `EnableLogicalSeek`,
`SetTimeout`, `SetWriteThreshold`.

Low level: `MTOP(op, count)` plus the exported `Op*` operation constants
(`OpFSF`, `OpWEOF`, `OpSeek`, …) and `Opt*` driver-option masks
(`OptSCSI2Logical`, `OptTwoFM`, …) for use with `WithDriverOptions` /
`SetDriverBooleans` / `ClearDriverBooleans`.

## Error handling

Sentinels (match with `errors.Is`):

* `ErrFilemark` — filemark boundary crossed (end of the current file).
* `ErrEndOfData` — two consecutive filemarks; no more recorded data.
* `ErrEarlyWarning` — first write `ENOSPC`; a trailer write is still allowed.
* `ErrEndOfMedium` — physical end of medium reached.
* `ErrNotOpen` — method called after `Close`.

## Platform

Linux only (the `st` driver). The ioctl request numbers are computed from the
asm-generic `_IOC` encoding — the layout used on **x86, amd64, arm64,
riscv64, loong64 and s390x**. MIPS, PowerPC and SPARC use a different `_IOC`
encoding and are not supported without arch-specific variants.

The `struct mtget` wire layout matches the kernel/glibc UAPI (the final
`mt_fileno` / `mt_blkno` fields are `__daddr_t`, i.e. 4-byte `int`, so
`sizeof == 48` and `MTIOCGET == 0x80306d02`).

## References

* `Documentation/scsi/st.rst` — the Linux kernel SCSI tape driver docs.
* `man 4 st` — the st driver manual page.
* `include/uapi/linux/mtio.h` — `struct mtop`, `struct mtget`, `struct mtpos`,
  the `MT*` operations and `MT_ST_*` option bits.
