//go:build linux

package tapedrive

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

type sgIO struct {
	InterfaceID  int32
	DxferDir     int32
	CmdLen       uint8
	MxSBLen      uint8
	IovecCount   uint16
	DxferLen     uint32
	Dxferp       *byte
	Cmdp         *byte
	Sbp          *byte
	Timeout      uint32
	Flags        uint32
	PackID       int32
	UsrPtr       *byte
	Status       uint8
	MaskedStatus uint8
	MsgStatus    uint8
	SbLenWr      uint8
	HostStatus   uint16
	DriverStatus uint16
	Resid        int32
	Duration     uint32
	Info         uint32
}

const (
	ioctlSGIO    = 0x2285
	ifaceMagic   = 'S'
	dxferNone    = -1
	dxferToDev   = -2
	dxferFromDev = -3

	sgTimeoutDefault = 30 * 1000
)

type SenseError struct {
	Sense []byte
	Key   uint8
	ASC   uint8
	ASCQ  uint8
}

func (e *SenseError) Error() string {
	return fmt.Sprintf("tapedrive: scsi check condition: key=0x%x asc=0x%02x ascq=0x%02x (sense=% x)", e.Key, e.ASC, e.ASCQ, e.Sense)
}

func (d *Drive) scsi(cdb, buf []byte, fromDevice bool, timeoutMs uint32) ([]byte, error) {
	if d.f == nil {
		return nil, errDriveClosed
	}
	if timeoutMs == 0 {
		timeoutMs = sgTimeoutDefault
	}
	var sense [64]byte
	var dir int32 = dxferNone
	switch {
	case fromDevice:
		dir = dxferFromDev
	case len(buf) > 0:
		dir = dxferToDev
	}
	req := sgIO{
		InterfaceID: ifaceMagic,
		DxferDir:    dir,
		CmdLen:      uint8(len(cdb)),
		MxSBLen:     uint8(len(sense)),
		DxferLen:    uint32(len(buf)),
		Cmdp:        &cdb[0],
		Sbp:         &sense[0],
		Timeout:     timeoutMs,
	}
	if len(buf) > 0 {
		req.Dxferp = &buf[0]
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), uintptr(ioctlSGIO), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return nil, fmt.Errorf("tapedrive: SG_IO ioctl: %w (status=0x%02x host=%d driver=%d)",
			errno, req.Status, req.HostStatus, req.DriverStatus)
	}
	if req.Status == 0x02 || req.SbLenWr > 0 {
		se := &SenseError{Sense: append([]byte(nil), sense[:req.SbLenWr]...)}
		if len(se.Sense) >= 14 {
			se.Key = se.Sense[2] & 0x0f
			se.ASC = se.Sense[12]
			se.ASCQ = se.Sense[13]
		}
		return nil, se
	}
	used := min(max(len(buf)-int(req.Resid), 0), len(buf))
	return buf[:used], nil
}

var errDriveClosed = errors.New("tapedrive: drive is closed")
