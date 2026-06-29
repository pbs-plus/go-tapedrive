//go:build linux

package tapedrive

import (
	"fmt"
	"strings"
)

type Inquiry struct {
	Vendor    string
	Product   string
	Revision  string
	IsChanger bool
	IsTape    bool
	Serial    string
}

const scsiInquiryCmd = 0x12

func trimSpaceASCII(b []byte) string {
	i := 0
	for i < len(b) && b[i] != 0 {
		i++
	}
	s := string(b[:i])
	return strings.TrimSpace(s)
}

func (d *Drive) scsiInquiryStandard() (Inquiry, error) {
	const allocLen = 96
	cdb := []byte{scsiInquiryCmd, 0, 0, 0, allocLen, 0}
	data, err := d.scsi(cdb, make([]byte, allocLen), true, sgTimeoutDefault)
	if err != nil {
		return Inquiry{}, fmt.Errorf("tapedrive: INQUIRY: %w", err)
	}
	if len(data) < 36 {
		return Inquiry{}, fmt.Errorf("tapedrive: INQUIRY: short response (%d bytes)", len(data))
	}
	in := Inquiry{
		Vendor:   trimSpaceASCII(data[8:16]),
		Product:  trimSpaceASCII(data[16:32]),
		Revision: trimSpaceASCII(data[32:36]),
	}
	peripheral := data[0]
	switch peripheral & 0x1f {
	case 1:
		in.IsTape = true
	case 8:
		in.IsChanger = true
	}
	return in, nil
}

func (d *Drive) scsiInquiryVPD(page byte, allocLen int) ([]byte, error) {
	cdb := []byte{scsiInquiryCmd, 0x01, page, 0, byte(allocLen), 0}
	data, err := d.scsi(cdb, make([]byte, allocLen), true, sgTimeoutDefault)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (d *Drive) Inquiry() (Inquiry, error) {
	in, err := d.scsiInquiryStandard()
	if err != nil {
		return Inquiry{}, err
	}
	if data, vpdErr := d.scsiInquiryVPD(0x80, 64); vpdErr == nil && len(data) >= 4 {
		pageLen := int(data[3])
		if 4+pageLen <= len(data) {
			in.Serial = trimSpaceASCII(data[4 : 4+pageLen])
		}
	}
	return in, nil
}
