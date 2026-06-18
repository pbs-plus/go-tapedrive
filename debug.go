//go:build linux

package tapedrive

import "unsafe"

// MTIOCTOP and MTIOCGET are the computed ioctl request numbers (built from the
// _IOW/_IOR macros so the embedded struct-size field is correct). Exported for
// diagnostics and for callers issuing raw ioctls.
var (
	MTIOCTOP = mtioctop
	MTIOCGET = mtioCget
)

// MtopSize and MtgetSize report the on-wire struct sizes used to build the
// ioctl request numbers. Useful when debugging an EINVAL from the kernel.
func MtopSize() uintptr  { return unsafe.Sizeof(mtop{}) }
func MtgetSize() uintptr { return unsafe.Sizeof(mtget{}) }
