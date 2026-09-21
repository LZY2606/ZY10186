package dbase

import (
	"errors"
	"fmt"
	"runtime"
)

var (
	// ErrEOF is returned when the end of a dBase database file is reached
	ErrEOF = errors.New("EOF")
	// ErrBOF is returned when the row pointer is attempted to be moved before the first row
	ErrBOF = errors.New("BOF")
	// ErrIncomplete is returned when the read of a row or column did not finish
	ErrIncomplete = errors.New("INCOMPLETE")
	// ErrNoFPT is returned when a file operation is attempted on a non-existent FPT file
	ErrNoFPT = errors.New("FPT_FILE_NOT_FOUND")
	// ErrNoDBF is returned when a file operation is attempted on a non-existent DBF file
	ErrNoDBF = errors.New("DBF_FILE_NOT_FOUND")
	// ErrInvalidPosition is returned when an invalid column position is used (x<1 or x>number of columns)
	ErrInvalidPosition = errors.New("INVALID_POSITION")
	// ErrInvalidEncoding is returned when an invalid encoding is encountered
	ErrInvalidEncoding = errors.New("INVALID_ENCODING")
	// ErrUnknownDataType is returned when an invalid data type is used
	ErrUnknownDataType = errors.New("UNKNOWN_DATA_TYPE")
	// ErrClosed is returned when an operation is attempted on a closed table.
	ErrClosed = errors.New("DBF_CLOSED")
	// ErrRowTruncated is returned when a record slot is shorter than the header declared row length.
	ErrRowTruncated = errors.New("ROW_TRUNCATED")
	// ErrTableTruncated is returned when the file ends before all rows declared in the header.
	ErrTableTruncated = errors.New("TABLE_TRUNCATED")
	// ErrInvalidMarker is returned when a record does not start with the active (0x20) or deleted (0x2A) marker.
	ErrInvalidMarker = errors.New("INVALID_DELETE_MARKER")
	// ErrMemoOutOfBounds is returned when a memo pointer references a block outside the memo file.
	ErrMemoOutOfBounds = errors.New("MEMO_OUT_OF_BOUNDS")
	// ErrMemoFree is returned when a memo pointer references the free space beyond the last used block.
	ErrMemoFree = errors.New("MEMO_FREE_BLOCK")
)

// Error is a wrapper for errors that occur in the dbase package
type Error struct {
	trace   []string
	details []error
	msg     string
}

// NewError creates a new Error with the given error message.
func NewError(err string) Error {
	e := Error{
		msg:     err,
		trace:   make([]string, 0),
		details: make([]error, 0),
	}
	e.trace = traceError(e)
	return e
}

// NewErrorf creates a new Error with formatted message using fmt.Sprintf.
func NewErrorf(format string, a ...interface{}) Error {
	e := Error{
		msg:     fmt.Sprintf(format, a...),
		trace:   make([]string, 0),
		details: make([]error, 0),
	}
	e.trace = traceError(e)
	return e
}

// Details adds an additional error detail to this Error.
func (e Error) Details(err error) Error {
	e.details = append(e.details, err)
	return e
}

func (e Error) Error() string {
	details := ""
	for _, d := range e.details {
		details += "=> " + d.Error()
	}

	if debug && len(e.trace) > 0 {
		trace := ""
		for i := len(e.trace) - 1; i >= 0; i-- {
			trace += e.trace[i]
			if i > 0 {
				trace += " -> "
			}
		}

		return fmt.Sprintf("%s: %s %s", trace, e.msg, details)
	}

	return fmt.Sprintf("%s %s", e.msg, details)
}

// Unwrap returns all wrapped detail errors so errors.Is/errors.As traverse the
// detailed causes as well.
func (e Error) Unwrap() []error {
	if len(e.details) == 0 {
		return nil
	}
	return e.details
}

// WrapError wraps an existing error into a dbase Error with trace information.
func WrapError(err error) Error {
	if err == nil {
		return NewError("unknown error occurred - cant wrap nil error")
	}
	if e, ok := err.(Error); ok {
		e.trace = traceError(e)
		return e
	}
	e := Error{
		msg:     err.Error(),
		trace:   make([]string, 0),
		details: make([]error, 0),
	}
	e.trace = traceError(e)
	return e
}

func traceError(e Error) []string {
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		return e.trace
	}

	e.trace = append(e.trace, fmt.Sprintf("%s:%d", file, line))
	return e.trace
}
