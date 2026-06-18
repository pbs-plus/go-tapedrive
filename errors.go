package tapedrive

import "errors"

// Sentinel errors returned by this package. Wrap with %w and match with
// errors.Is. They reflect the st driver semantics documented in st.rst.
var (
	// ErrEndOfData is returned when a read returns zero bytes twice in a row:
	// the first zero is a filemark (reported as io.EOF), the second means
	// there is no more recorded data on the tape.
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
