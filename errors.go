package tapedrive

import "errors"

// Sentinel errors returned by this package. Wrap with %w and match with
// errors.Is. They correspond to the errno values documented in st.rst.
var (
	// ErrEndOfData is returned when a read encounters two consecutive
	// filemarks, i.e. there is no more recorded data on the tape.
	ErrEndOfData = errors.New("tapedrive: end of recorded data")
	// ErrEndOfMedium is returned when a write hits the physical end of
	// medium (after the early-warning window and trailer write are used up).
	ErrEndOfMedium = errors.New("tapedrive: end of medium")
	// ErrNoTape is returned when there is no tape loaded in the drive
	// (GMT_DR_OPEN / door open).
	ErrNoTape = errors.New("tapedrive: no tape in drive")
	// ErrWriteProtected is returned when the loaded tape is write protected.
	ErrWriteProtected = errors.New("tapedrive: tape is write protected")
	// ErrNotOpen is returned by methods used after Close.
	ErrNotOpen = errors.New("tapedrive: device not open")
	// ErrCleanRequested indicates the drive reports a cleaning request.
	ErrCleanRequested = errors.New("tapedrive: cleaning requested")
)
