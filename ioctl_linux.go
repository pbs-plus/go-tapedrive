package tapedrive

import (
	"syscall"
	"unsafe"
)

// ioctl issues a Linux ioctl via the raw syscall. It is the single point of
// contact with the kernel for the MTIOC* commands and never allocates: the
// arg pointer must point at a caller-owned value (stack or struct field).
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// kernelOps is the production tapeOps implementation backed by a real fd.
// All methods are zero-allocation: they pass the caller-owned argument
// pointers straight into the kernel via unsafe.Pointer.
type kernelOps struct{ fd int }

func (k kernelOps) ioctlTop(op *mtop) error     { return ioctl(k.fd, ioctlMTIOCTOP, unsafe.Pointer(op)) }
func (k kernelOps) ioctlGet(g *mtget) error     { return ioctl(k.fd, ioctlMTIOCGET, unsafe.Pointer(g)) }
func (k kernelOps) ioctlPos(p *mtpos) error     { return ioctl(k.fd, ioctlMTIOCPOS, unsafe.Pointer(p)) }
func (k kernelOps) read(p []byte) (int, error)  { return syscall.Read(k.fd, p) }
func (k kernelOps) write(p []byte) (int, error) { return syscall.Write(k.fd, p) }
func (k kernelOps) close() error                { return syscall.Close(k.fd) }
