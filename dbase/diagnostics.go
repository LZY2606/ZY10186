package dbase

import "io"

// validateDeclaredSize checks that the backing store is large enough for the
// number of records declared in the DBF header. A size of -1 means the store
// size cannot be determined (custom readers without io.Seeker) and skips the
// check; short reads then surface as ErrRowTruncated later.
func validateDeclaredSize(file *File, size int64) error {
	if size < 0 || file.header == nil {
		return nil
	}
	expected := int64(file.header.FirstRow) + int64(file.header.RowsCount)*int64(file.header.RowLength)
	if size < expected {
		return NewErrorf("table truncated: file ends at %d bytes, header declares %d rows ending at %d bytes",
			size, file.header.RowsCount, expected).Details(ErrTableTruncated)
	}
	return nil
}

// seekerSize returns the current size of an io.Seeker based store and restores
// the previous seek position.
func seekerSize(seeker io.Seeker) (int64, error) {
	previous, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	size, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := seeker.Seek(previous, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}

// memoStoreSize returns the memo backing store size, or -1 if it cannot be
// determined. Seek position is restored.
func memoStoreSize(handle io.ReadWriteSeeker) int64 {
	if seeker, ok := handle.(io.Seeker); ok {
		if size, err := seekerSize(seeker); err == nil {
			return size
		}
	}
	return -1
}

// firstMemoDataBlock returns the block number of the first usable memo block.
// The FPT header always occupies the first 512 bytes regardless of block size,
// matching the Visual FoxPro layout (block 8 with the common 64 byte blocks).
func firstMemoDataBlock(blockSize uint16) uint32 {
	if blockSize == 0 {
		return 1
	}
	first := uint32(512) / uint32(blockSize)
	if uint32(512)%uint32(blockSize) != 0 {
		first++
	}
	return first
}

// memoBlocksNeeded calculates how many blocks a memo entry of rawLen payload
// bytes occupies, including the 8 byte per-entry header.
func memoBlocksNeeded(rawLen int, blockSize uint16) uint32 {
	if blockSize == 0 {
		return 1
	}
	need := uint32(rawLen) + 8
	blocks := need / uint32(blockSize)
	if need%uint32(blockSize) != 0 {
		blocks++
	}
	return blocks
}

// validateMemoPointer distinguishes memo pointers into unallocated/free space
// from pointers beyond the memo file. nextFree is the first free block, size is
// the backing store size (-1 if unknown).
func validateMemoPointer(file *File, block uint32, size int64) error {
	first := firstMemoDataBlock(file.memoHeader.BlockSize)
	if block < first {
		return NewErrorf("memo pointer block %d is inside the memo header (first data block is %d)",
			block, first).Details(ErrMemoOutOfBounds)
	}
	if block < file.memoHeader.NextFree {
		return nil
	}
	entryOffset := int64(file.memoHeader.BlockSize) * int64(block)
	if size >= 0 && entryOffset+8 > size {
		return NewErrorf("memo pointer block %d at byte offset %d is beyond the memo file size %d",
			block, entryOffset, size).Details(ErrMemoOutOfBounds)
	}
	return NewErrorf("memo pointer block %d is beyond the first free block %d (references free/unwritten space)",
		block, file.memoHeader.NextFree).Details(ErrMemoFree)
}
