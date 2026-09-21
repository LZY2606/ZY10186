package dbase

import "io"

// IO is the interface for working with dBase files.
// It provides methods for opening, reading, writing, and managing dBase database files and their memo files.
// Three implementations are available:
// - WindowsIO (for direct file access on Windows)
// - UnixIO (for direct file access on Unix-like systems)
// - GenericIO (for custom file access implementing io.ReadWriteSeeker)
type IO interface {
	OpenTable(config *Config) (*File, error)
	Close(file *File) error
	Create(file *File) error
	ReadHeader(file *File) error
	WriteHeader(file *File) error
	ReadColumns(file *File) ([]*Column, *Column, error)
	WriteColumns(file *File) error
	ReadMemoHeader(file *File) error
	WriteMemoHeader(file *File, size int) error
	ReadMemo(file *File, address []byte, column *Column) ([]byte, bool, error)
	WriteMemo(address []byte, file *File, raw []byte, text bool, length int) ([]byte, error)
	ReadNullFlag(file *File, position uint64, column *Column) (bool, bool, error)
	ReadRow(file *File, position uint32) ([]byte, error)
	WriteRow(file *File, row *Row) error
	Search(file *File, field *Field, exactMatch bool) ([]*Row, error)
	GoTo(file *File, row uint32) error
	Skip(file *File, offset int64)
	Deleted(file *File) (bool, error)
}

// OpenTable opens a dBase database file (and the memo file if needed).
// The config parameter is required to specify either:
//   - IO: custom IO implementation (takes priority if provided)
//   - Data: DBF file content as bytes (with optional MemoData for FPT content)
//   - Reader: DBF file content as io.ReadWriteSeeker (with optional MemoReader)
//   - Filename: path to DBF file on filesystem (fallback option)
//
// If no IO is provided, one will be created based on available data sources.
func OpenTable(config *Config) (*File, error) {
	if config == nil {
		return nil, NewError("missing dbase configuration")
	}

	// Validate that exactly one data source is provided
	if err := config.validateDataSources(); err != nil {
		return nil, err
	}

	// If custom IO is already provided, use it directly
	if config.IO != nil {
		return config.IO.OpenTable(config)
	}

	// No custom IO provided, so create one based on available data sources
	if config.Data != nil || config.Reader != nil {
		// Create GenericIO for byte/reader data
		var dbfHandle, memoHandle io.ReadWriteSeeker

		if config.Reader != nil {
			dbfHandle = config.Reader
			memoHandle = config.MemoReader
		}

		if config.Data != nil {
			dbfHandle = NewBytesReadWriteSeeker(config.Data)
			if config.MemoData != nil {
				memoHandle = NewBytesReadWriteSeeker(config.MemoData)
			}
		}

		// Create a copy of config with GenericIO
		configCopy := *config
		configCopy.IO = GenericIO{
			Handle:        dbfHandle,
			RelatedHandle: memoHandle,
		}

		return configCopy.IO.OpenTable(&configCopy)
	}

	// Fall back to filesystem access with DefaultIO
	if config.Filename == "" {
		return nil, NewError("missing filename, data, or reader in configuration")
	}

	config.IO = DefaultIO
	return config.IO.OpenTable(config)
}

// Close closes all file handlers for the dBase file and its associated memo file.
func (file *File) Close() error {
	end, err := file.beginClose()
	if err != nil {
		return WrapError(err)
	}
	closeErr := file.io.Close(file)
	end()
	if closeErr != nil {
		// A failed close leaves the handle state unknown; keep closed=true so
		// callers do not keep using a half closed file and return the error.
		return WrapError(closeErr)
	}
	return nil
}

// Create creates a new dBase database file (and the memo file if needed).
func (file *File) Create() error {
	// Create is part of NewTable/Init before the file is published; it does
	// not take the lifecycle lock.
	file.isNew = true
	return file.defaults().io.Create(file)
}

// ReadHeader reads the dBase file header from the file handle.
func (file *File) ReadHeader() error {
	end, err := file.beginRead()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	return file.io.ReadHeader(file)
}

// WriteHeader writes the header to the dBase file.
func (file *File) WriteHeader() error {
	end, err := file.beginWrite()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	return file.io.WriteHeader(file)
}

// ReadColumns reads column definitions from the dBase file header, starting at position 32,
// until it finds the header row terminator END_OF_COLUMN (0x0D).
func (file *File) ReadColumns() ([]*Column, *Column, error) {
	end, err := file.beginRead()
	if err != nil {
		return nil, nil, WrapError(err)
	}
	defer end()
	return file.io.ReadColumns(file)
}

// WriteColumns writes the column definitions to the end of the header in the dBase file.
func (file *File) WriteColumns() error {
	end, err := file.beginWrite()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	return file.io.WriteColumns(file)
}

// ReadMemoHeader reads the memo file header from the given file handle.
func (file *File) ReadMemoHeader() error {
	end, err := file.beginRead()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	return file.io.ReadMemoHeader(file)
}

// WriteMemoHeader writes the memo header to the memo file.
// The size parameter specifies the number of blocks the new memo data will occupy.
func (file *File) WriteMemoHeader(size int) error {
	end, err := file.beginWrite()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	return file.io.WriteMemoHeader(file, size)
}

// ReadRow reads the raw row data of one row at the specified row position.
func (file *File) ReadRow(position uint32) ([]byte, error) {
	end, err := file.beginRead()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	return file.io.ReadRow(file, position)
}

// WriteRow writes the raw row data to the specified row position in the dBase file.
func (file *File) WriteRow(row *Row) error {
	end, err := file.beginWrite()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	return file.io.WriteRow(file, row)
}

// ReadMemo reads one or more blocks from the memo file for the specified memo column.
// Returns the raw data and a boolean indicating if the data is text (true) or binary (false).
func (file *File) ReadMemo(address []byte, column *Column) ([]byte, bool, error) {
	end, err := file.beginRead()
	if err != nil {
		return nil, false, WrapError(err)
	}
	defer end()
	return file.readMemoLocked(address, column)
}

// readMemoLocked reads a memo block while the lifecycle lock is already held.
func (file *File) readMemoLocked(address []byte, column *Column) ([]byte, bool, error) {
	return file.io.ReadMemo(file, address, column)
}

// WriteMemo writes memo data to the memo file and returns the address of the memo.
// The text parameter indicates whether the data is text (true) or binary (false).
// The length parameter specifies the length of the data to write.
func (file *File) WriteMemo(address []byte, data []byte, text bool, length int) ([]byte, error) {
	end, err := file.beginWrite()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	return file.writeMemoLocked(address, data, text, length)
}

// writeMemoLocked writes a memo block while the lifecycle lock is held.
func (file *File) writeMemoLocked(address []byte, data []byte, text bool, length int) ([]byte, error) {
	return file.io.WriteMemo(address, file, data, text, length)
}

// ReadNullFlag reads the null flag field at the end of the row.
// The null flag field indicates if the field has a variable length.
// Returns true as the first value if the field is variable length, and true as the second value if the field is null.
func (file *File) ReadNullFlag(position uint64, column *Column) (bool, bool, error) {
	end, err := file.beginRead()
	if err != nil {
		return false, false, WrapError(err)
	}
	defer end()
	return file.readNullFlagLocked(position, column)
}

// readNullFlagLocked reads the null flag while the lifecycle lock is held.
func (file *File) readNullFlagLocked(position uint64, column *Column) (bool, bool, error) {
	return file.io.ReadNullFlag(file, position, column)
}

// Search searches for rows that contain the specified value in the given field.
// If exactMatch is true, only exact matches are returned; otherwise, partial matches are included.
func (file *File) Search(field *Field, exactMatch bool) ([]*Row, error) {
	end, err := file.beginWrite()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	return file.io.Search(file, field, exactMatch)
}

// GoTo sets the internal row pointer to the specified row number.
// Returns an EOF error if positioning beyond the end of file and positions the pointer at lastRow+1.
func (file *File) GoTo(row uint32) error {
	end, err := file.beginWrite()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	return file.io.GoTo(file, row)
}

// Skip adds the specified offset to the internal row pointer.
// If the result would position beyond the end of file, positions the pointer at lastRow+1.
// If the result would be negative, positions the pointer at 0.
// Note: This method does not skip deleted rows automatically.
func (file *File) Skip(offset int64) {
	end, err := file.beginWrite()
	if err != nil {
		return
	}
	defer end()
	file.io.Skip(file, offset)
}

// Deleted returns true if the row at the current internal row pointer position is marked as deleted.
func (file *File) Deleted() (bool, error) {
	end, err := file.beginWrite()
	if err != nil {
		return false, WrapError(err)
	}
	defer end()
	return file.io.Deleted(file)
}

// GetIO returns the IO implementation currently being used by this file.
func (file *File) GetIO() IO {
	end, _ := file.beginRead()
	if end == nil {
		return nil
	}
	defer end()
	return file.io
}

// GetHandle returns the file handles being used (dBase file handle, memo file handle).
func (file *File) GetHandle() (interface{}, interface{}) {
	end, _ := file.beginRead()
	if end == nil {
		return nil, nil
	}
	defer end()
	return file.handle, file.relatedHandle
}

// Sets the default if no io is set
func (file *File) defaults() *File {
	if file.io == nil {
		file.io = DefaultIO
	}
	return file
}

// ValidateFileVersion checks if the dBase file version is supported and tested.
// If untested is true, validation is bypassed and any version is accepted.
func ValidateFileVersion(version byte, untested bool) error {
	if untested {
		return nil
	}
	debugf("Validating file version: %d", version)
	switch version {
	default:
		return NewErrorf("untested DBF file version: %d (0x%x)", version, version)
	case byte(FoxPro), byte(FoxProAutoincrement), byte(FoxProVar):
		return nil
	}
}
