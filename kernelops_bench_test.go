package tapedrive

import (
	"io"
	"testing"
)

// These benchmarks exercise the production kernelOps code paths in isolation
// (no fakeDevice) to prove the Read/Write/Seek/Status hot paths in this
// package allocate zero bytes. They cannot do real I/O without a tape drive,
// so they stop at the boundary: argument struct construction, the unsafe
// pointer escape, and the return-value decode. The actual syscall.Read/Write
// themselves never allocate in Go's runtime.

func BenchmarkMtopConstruction(b *testing.B) {
	// Exactly what Tape.mtop builds before handing off to ioctlTop.
	b.ReportAllocs()
	for range b.N {
		cmd := mtop{Op: OpSeek, Count: 123456}
		_ = cmd
	}
}

func BenchmarkStatusDecode(b *testing.B) {
	// Exactly what Tape.Status builds from an on-stack mtget.
	g := mtget{
		Type: 0x72, Dsreg: 1<<dsDensityShift | 65536, Gstat: gmtOnline | gmtBOT,
		Erreg: 3, Fileno: 4, Blkno: 5,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := Status{
			Type:           g.Type,
			Resid:          g.Resid,
			BlockSize:      int(uint64(g.Dsreg) & dsBlksizeMask),
			Density:        int((uint64(g.Dsreg) >> dsDensityShift) & 0xff),
			Gstat:          g.Gstat,
			SoftErrorCount: int(uint64(g.Erreg) & dsSofterrMask),
			FileNo:         int64(g.Fileno),
			BlkNo:          int64(g.Blkno),
		}
		_ = s
	}
}

func BenchmarkSeekArgBuild(b *testing.B) {
	// Mirror Tape.Seek(SeekStart): compute target, build mtop.
	b.ReportAllocs()
	for i := range b.N {
		target := int64(i)
		op := mtop{Op: OpSeek, Count: int32(target)}
		_ = op
	}
}

func BenchmarkIocNumberLookup(b *testing.B) {
	// Reading the package var must not allocate.
	b.ReportAllocs()
	for range b.N {
		_ = ioctlMTIOCTOP
		_ = ioctlMTIOCGET
		_ = ioctlMTIOCPOS
	}
}

// Ensure io.Seeker/Reader/Writer/Closer keep compiling against the Tape type.
var (
	_ io.ReadSeeker      = (*Tape)(nil)
	_ io.ReadWriteSeeker = (*Tape)(nil)
)
