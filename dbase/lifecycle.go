package dbase

import (
	"bytes"
	"encoding/binary"
	"io"
)

// Lifecycle and concurrency boundary.
//
// A File owns exactly two on-disk resources: the DBF handle (file.handle) and,
// when the table has memo columns, the FPT handle (file.relatedHandle). The
// lifecycle RWMutex serializes every public entry point that touches those
// handles or the in-memory cursor/header state:
//
//   - read entries take a read lock, so two reads never observe a half-written
//     header or a moved shared file offset;
//   - write entries and Close take the write lock, so a write never overlaps a
//     read or another write and Close waits for every in-flight operation.
//
// Cursor movement (GoTo, Skip) mutates table.rowPointer and therefore also
// takes the write lock: row-pointer based reads (Row/Next) are safe against
// writes but two goroutines still must not share one cursor. Position based
// reads (ReadRowAt) are independent of the cursor and may run concurrently.
//
// Methods named ...Locked expect the lifecycle lock to already be held and are
// used internally to avoid re-entrant locking.

// beginRead acquires the lifecycle lock for a read operation.
// It returns the end function to defer, or ErrClosed if Close already ran.
func (file *File) beginRead() (func(), error) {
	file.opMu.RLock()
	if file.closed {
		file.opMu.RUnlock()
		return nil, ErrClosed
	}
	return file.opMu.RUnlock, nil
}

// beginWrite acquires the lifecycle lock for a write operation.
func (file *File) beginWrite() (func(), error) {
	file.opMu.Lock()
	if file.closed {
		file.opMu.Unlock()
		return nil, ErrClosed
	}
	return file.opMu.Unlock, nil
}

// beginClose marks the file closed while holding the write lock and returns an
// unlock function. A second Close fails with ErrClosed.
func (file *File) beginClose() (func(), error) {
	file.opMu.Lock()
	if file.closed {
		file.opMu.Unlock()
		return nil, ErrClosed
	}
	return func() {
		file.closed = true
		file.opMu.Unlock()
	}, nil
}

// expectSize is the minimum physical DBF size required to read the record at
// position. A smaller file means the header record count points past the data
// that actually exists.
func expectRowEnd(header *Header, position uint32) int64 {
	return int64(header.FirstRow) + int64(position+1)*int64(header.RowLength)
}

// validateDbfSize distinguishes "header declares more records than the file
// holds" from a truncated single record. When the declared table size exceeds
// the physical size it returns ErrRecordCountMismatch.
func validateDbfSize(header *Header, physicalSize int64, position uint32) error {
	if header.FileSize() > physicalSize {
		return NewErrorf("header declares %d records requiring %d bytes but DBF is only %d bytes",
			header.RowsCount, header.FileSize(), physicalSize).Details(ErrRecordCountMismatch)
	}
	return nil
}

// validateRowRead maps a short read of one record to ErrTruncatedRecord.
func validateRowRead(read, want int, position uint32) error {
	if read != want {
		return NewErrorf("truncated record at position %d: read %d bytes, expected %d", position, read, want).
			Details(ErrTruncatedRecord)
	}
	return nil
}

// memoBlockOf returns the memo block number encoded in a record's memo field.
func memoBlockOf(address []byte) uint32 {
	return binary.LittleEndian.Uint32(address)
}

// validateMemoAddress classifies a memo pointer.
//
//   - an all-zero pointer is the empty memo marker and allowed;
//   - a pointer into the reserved header region (< first data block) is
//     ErrMemoFreeBlock: it can only reference header/free space, never data;
//   - a pointer whose data header starts at or beyond the physical FPT end is
//     ErrMemoOutOfBounds.
func validateMemoAddress(header *MemoHeader, physicalSize int64, address []byte) error {
	if isEmptyBytes(address) {
		return nil
	}
	block := memoBlockOf(address)
	if header == nil || header.BlockSize == 0 {
		return NewErrorf("memo block size is not defined").Details(ErrMemoFreeBlock)
	}
	firstDataBlock := memoHeaderBlocks(header.BlockSize)
	if block < firstDataBlock {
		return NewErrorf("memo pointer references reserved/free block %d (first data block is %d)",
			block, firstDataBlock).Details(ErrMemoFreeBlock)
	}
	offset := int64(block) * int64(header.BlockSize)
	if offset+8 > physicalSize {
		return NewErrorf("memo pointer block %d at offset %d is beyond the FPT end (%d bytes)",
			block, offset, physicalSize).Details(ErrMemoOutOfBounds)
	}
	return nil
}

// memoHeaderBlocks is the number of blocks the 512 byte FPT header occupies.
// FoxPro uses 64 byte blocks so the first data block is block 8. A 512 byte
// block size (dBase IV style) keeps the first data block at block 1.
func memoHeaderBlocks(blockSize uint16) uint32 {
	if blockSize == 0 {
		return 0
	}
	return uint32(512 / blockSize)
}

// seekSize returns the current size of a seekable handle and restores the
// previous offset. It is used by the IO implementations for physical size
// validation; a failed restore leaves the error to the caller, the handle is
// never left with a deliberately moved offset on the success path.
func seekSize(handle io.Seeker) (int64, error) {
	current, err := handle.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := handle.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := handle.Seek(current, io.SeekStart); err != nil {
		return 0, err
	}
	return end, nil
}

// clampPointer implements the documented GoTo/Skip clamping without the
// wraparound that happened when a negative offset underflowed uint32.
func clampPointer(current uint32, offset int64, rowsCount uint32) uint32 {
	next := int64(current) + offset
	switch {
	case next < 0:
		return 0
	case next >= int64(rowsCount):
		return rowsCount
	default:
		return uint32(next)
	}
}

// searchAt is the cursor based, IO independent Search implementation used by
// all three IO backends while the lifecycle write lock is held. It scans raw
// record bytes directly, so a matching row is decoded once instead of moving
// the cursor and re-reading through the gated public entry points.
func searchAt(file *File, field *Field, exactMatch bool) ([]*Row, error) {
	if field == nil || field.column == nil {
		return nil, NewError("search field is not defined")
	}
	if field.column.DataType == 'M' {
		return nil, NewError("searching memo fields is not supported")
	}
	raw, err := file.representLocked(field, !exactMatch)
	if err != nil {
		return nil, WrapError(err)
	}
	rows := make([]*Row, 0)
	for i := uint32(0); i < file.header.RowsCount; i++ {
		data, rerr := file.io.ReadRow(file, i)
		if rerr != nil {
			continue
		}
		start := field.column.Position
		end := int(start) + int(field.column.Length)
		if end > len(data) {
			continue
		}
		haystack := data[start:end]
		match := false
		if exactMatch {
			match = bytesTrimEqual(haystack, raw)
		} else {
			match = bytesContains(haystack, raw)
		}
		if !match {
			continue
		}
		file.table.rowPointer = i
		row, derr := file.bytesToRowLocked(data)
		if derr != nil {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func bytesContains(haystack, needle []byte) bool {
	return bytes.Contains(haystack, needle)
}

// bytesTrimEqual compares exact matches ignoring trailing padding spaces,
// matching the old partial-search padding tolerance for exact searches.
func bytesTrimEqual(a, b []byte) bool {
	return bytes.Equal(bytes.TrimRight(a, " "), bytes.TrimRight(b, " "))
}
