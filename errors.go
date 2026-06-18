package tapedrive

import "errors"

// Sentinel errors returned by this package. Wrap with %w and match with
// errors.Is. They reflect the st driver semantics documented in st.rst.
var (
	// ErrFilemark is returned by ReadBlock when the drive reports a filemark:
	// no data was read and the tape is now positioned at the first block of
	// the next file. A subsequent ReadBlock returns that block.
	ErrFilemark = errors.New("tapedrive: filemark")
	// ErrEndOfData is returned by ReadBlock when two filemarks appear in a
	// row with no data between them: there is no more recorded data on the
	// tape. Any tape-movement op resets the filemark counter so a lone
	// filemark after a reposition stays an ErrFilemark.
	ErrEndOfData = errors.New("tapedrive: end of recorded data")
	// ErrEarlyWarning is returned on the first write that fails with ENOSPC.
	// Per st.rst the early-warning window still allows one trailer write; the
	// caller may retry once before treating the medium as full.
	ErrEarlyWarning = errors.New("tapedrive: end of medium early warning")
	// ErrEndOfMedium is returned once the early-warning window and the trailer
	// write are exhausted, i.e. the physical end of medium has been reached.
	ErrEndOfMedium = errors.New("tapedrive: end of medium")
	// ErrNotOpen is returned by methods used after Close.
	ErrNotOpen = errors.New("tapedrive: device not open")
)
