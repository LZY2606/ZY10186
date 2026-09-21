package dbase

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// File is the main struct to handle a dBase file.
// Each file type is basically a Table or a Memo file.
type File struct {
	config         *Config     // The config used when working with the DBF file.
	handle         interface{} // DBase file handle.
	relatedHandle  interface{} // Memo file handle.
	io             IO          // The IO interface used to work with the DBF file.
	header         *Header     // DBase file header containing relevant information.
	memoHeader     *MemoHeader // Memo file header containing relevant information.
	dbaseMutex     *sync.Mutex // Mutex locks for concurrent writing access to the DBF file.
	memoMutex      *sync.Mutex // Mutex locks for concurrent writing access to the FPT file.
	table          *Table      // Containing the columns and internal row pointer.
	nullFlagColumn *Column     // The column containing the null flag column (if varchar or varbinary field exists).
	isNew          bool
	opMu           sync.RWMutex // Lifecycle lock: guards handles, header/cursor state and Close.
	closed         bool         // True once Close completed; further entry points fail with ErrClosed.
}

// TableName returns the name of the dBase table.
func (file *File) TableName() string {
	end, _ := file.beginRead()
	if end == nil {
		return ""
	}
	defer end()
	return file.table.name
}

// EOF returns true if the internal row pointer is at the end of file.
func (file *File) EOF() bool {
	end, _ := file.beginRead()
	if end == nil {
		return true
	}
	defer end()
	return file.table.rowPointer >= file.header.RowsCount
}

// BOF returns true if the internal row pointer is before the first row.
func (file *File) BOF() bool {
	end, _ := file.beginRead()
	if end == nil {
		return false
	}
	defer end()
	return file.table.rowPointer == 0
}

// Pointer returns the current row pointer position.
func (file *File) Pointer() uint32 {
	end, _ := file.beginRead()
	if end == nil {
		return 0
	}
	defer end()
	return file.table.rowPointer
}

// Header returns the dBase file header struct for inspection.
func (file *File) Header() *Header {
	end, _ := file.beginRead()
	if end == nil {
		return nil
	}
	defer end()
	return file.header
}

// RowsCount returns the number of rows in the dBase file.
func (file *File) RowsCount() uint32 {
	end, _ := file.beginRead()
	if end == nil {
		return 0
	}
	defer end()
	return file.header.RowsCount
}

// Columns returns all columns in the dBase table.
func (file *File) Columns() []*Column {
	end, _ := file.beginRead()
	if end == nil {
		return nil
	}
	defer end()
	return file.table.columns
}

// Column returns the column at the specified position, or nil if the position is invalid.
func (file *File) Column(pos int) *Column {
	if pos < 0 || pos >= len(file.table.columns) {
		return nil
	}
	return file.table.columns[pos]
}

// ColumnsCount returns the number of columns in the dBase table.
func (file *File) ColumnsCount() uint16 {
	return uint16(len(file.table.columns))
}

// ColumnNames returns a slice containing all column names in the dBase table.
func (file *File) ColumnNames() []string {
	num := len(file.table.columns)
	names := make([]string, num)
	for i := 0; i < num; i++ {
		names[i] = file.table.columns[i].Name()
	}
	return names
}

// ColumnPosByName returns the position of a column by name, or -1 if not found.
func (file *File) ColumnPosByName(colname string) int {
	for i := 0; i < len(file.table.columns); i++ {
		if file.table.columns[i].Name() == colname {
			return i
		}
	}
	return -1
}

// ColumnPos returns the position of the specified column, or -1 if not found.
func (file *File) ColumnPos(column *Column) int {
	for i := 0; i < len(file.table.columns); i++ {
		if file.table.columns[i] == column {
			return i
		}
	}
	return -1
}

// SetColumnModification sets a modification for the column at the specified position.
// If the position is out of range, the operation is ignored.
func (file *File) SetColumnModification(position int, mod *Modification) {
	// Skip if position is out of range
	if position < 0 || position >= len(file.table.columns) {
		return
	}
	debugf("Modification set for column %d", position)
	file.table.mods[position] = mod
}

// SetColumnModificationByName sets a modification for the column with the specified name.
// Returns an error if the column is not found.
func (file *File) SetColumnModificationByName(name string, mod *Modification) error {
	end, err := file.beginWrite()
	if err != nil {
		return WrapError(err)
	}
	defer end()
	position := file.columnPosByNameLocked(name)
	if position < 0 {
		return NewErrorf("Column '%s' not found", name)
	}
	file.SetColumnModification(position, mod)
	return nil
}

// GetColumnModification returns the column modification for the column at the specified position.
func (file *File) GetColumnModification(position int) *Modification {
	end, _ := file.beginRead()
	if end == nil {
		return nil
	}
	defer end()
	if position < 0 || position >= len(file.table.mods) {
		return nil
	}
	return file.table.mods[position]
}

// columnPosByNameLocked looks up a column position while the lifecycle lock is held.
func (file *File) columnPosByNameLocked(colname string) int {
	for i := 0; i < len(file.table.columns); i++ {
		if file.table.columns[i].Name() == colname {
			return i
		}
	}
	return -1
}

// Init creates the dBase files and writes the header and columns to them.
func (file *File) Init() error {
	err := file.Create()
	if err != nil {
		return err
	}
	err = file.WriteHeader()
	if err != nil {
		return err
	}
	err = file.WriteColumns()
	if err != nil {
		return err
	}
	if file.memoHeader != nil {
		err = file.WriteMemoHeader(0)
		if err != nil {
			return err
		}
	}

	return nil
}

// Rows returns all rows in the dBase file as a slice.
// If skipInvalid is true, invalid rows are skipped instead of returning an error.
// If skipDeleted is true, deleted rows are excluded from the result.
func (file *File) Rows(skipInvalid bool, skipDeleted bool) ([]*Row, error) {
	end, err := file.beginWrite()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	rows := make([]*Row, 0)
	for file.table.rowPointer < file.header.RowsCount {
		row, err := file.rowLocked()
		if err != nil {
			if skipInvalid {
				file.table.rowPointer = clampPointer(file.table.rowPointer, 1, file.header.RowsCount)
				continue
			}
			return nil, WrapError(err)
		}
		file.table.rowPointer = clampPointer(file.table.rowPointer, 1, file.header.RowsCount)

		// skip deleted rows
		if row.Deleted && skipDeleted {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Next reads the current row and increments the row pointer by one.
func (file *File) Next() (*Row, error) {
	end, err := file.beginWrite()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	row, err := file.rowLocked()
	if err != nil {
		return nil, WrapError(err)
	}
	file.table.rowPointer = clampPointer(file.table.rowPointer, 1, file.header.RowsCount)
	return row, nil
}

// Row returns the row at the current file row pointer position.
func (file *File) Row() (*Row, error) {
	// The row pointer is shared cursor state, so cursor based reads take the
	// write lock just like Next. Goroutines that need to read concurrently must
	// use ReadRowAt with an explicit position.
	end, err := file.beginWrite()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	return file.rowLocked()
}

// rowLocked reads the row at the current cursor while the lifecycle lock is
// already held by the caller.
func (file *File) rowLocked() (*Row, error) {
	data, err := file.io.ReadRow(file, file.table.rowPointer)
	if err != nil {
		return nil, WrapError(err)
	}
	return file.bytesToRowLocked(data)
}

// ReadRowAt reads one record at an explicit, zero based position without
// touching the shared row pointer. It is safe to call concurrently from
// multiple goroutines and from read-only tables.
func (file *File) ReadRowAt(position uint32) (*Row, error) {
	end, err := file.beginRead()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	data, err := file.io.ReadRow(file, position)
	if err != nil {
		return nil, WrapError(err)
	}
	return file.bytesToRowLocked(data)
}

// NewRow creates a new Row struct with the same column structure as the dBase file.
// The row is positioned at the next available row position.
func (file *File) NewRow() *Row {
	end, _ := file.beginRead()
	if end == nil {
		return nil
	}
	defer end()
	return file.newRowLocked()
}

// newRowLocked builds an empty row at the next append position while the
// lifecycle lock is held.
func (file *File) newRowLocked() *Row {
	row := &Row{
		handle:   file,
		Position: file.header.RowsCount + 1,
		Deleted:  false,
		fields:   make([]*Field, 0),
	}
	for _, column := range file.table.columns {
		row.fields = append(row.fields, &Field{
			column: column,
			value:  nil,
		})
	}
	debugf("Initiliazing new at position %d", row.Position)
	return row
}

// NewField creates a new field with the specified value and column at the given position.
// Returns an error if the column position is invalid.
func (file *File) NewField(pos int, value interface{}) (*Field, error) {
	end, _ := file.beginRead()
	if end == nil {
		return nil, WrapError(ErrClosed)
	}
	defer end()
	column := file.columnLocked(pos)
	if column == nil {
		return nil, NewErrorf("column at position %v not found", pos)
	}
	return &Field{column: column, value: value}, nil
}

// NewFieldByName creates a new field with the specified value and column identified by name.
// Returns an error if the column is not found.
func (file *File) NewFieldByName(name string, value interface{}) (*Field, error) {
	end, _ := file.beginRead()
	if end == nil {
		return nil, WrapError(ErrClosed)
	}
	defer end()
	pos := file.columnPosByNameLocked(name)
	if pos < 0 {
		return nil, NewErrorf("column '%s' not found", name)
	}
	return file.NewField(pos, value)
}

// BytesToRow converts raw row data to a Row struct.
// If the data references a memo (FPT) file, that file is also read.
func (file *File) BytesToRow(data []byte) (*Row, error) {
	end, err := file.beginRead()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	return file.bytesToRowLocked(data)
}

// bytesToRowLocked converts raw row bytes while the lifecycle lock is held.
// Varchar/varbinary null flags are read through the cursor independent IO
// helpers and therefore only work for the pointer based callers; all other
// column types are decoded purely from data.
func (file *File) bytesToRowLocked(data []byte) (*Row, error) {
	debugf("Converting row data (%d bytes) to row struct...", len(data))
	rec := &Row{}
	rec.Position = file.table.rowPointer
	rec.handle = file
	rec.fields = make([]*Field, 0)
	if len(data) < int(file.header.RowLength) {
		return nil, NewErrorf("truncated record at position %d: %d bytes < %d bytes",
			rec.Position, len(data), int(file.header.RowLength)).Details(ErrTruncatedRecord)
	}
	// a row should start with te delete flag, a space ACTIVE(0x20) or DELETED(0x2A)
	rec.Deleted = Marker(data[0]) == Deleted
	if !rec.Deleted && Marker(data[0]) != Active {
		return nil, NewErrorf("invalid delete flag 0x%02x at record %d: expected 0x20 (active) or 0x2a (deleted)",
			data[0], rec.Position).Details(ErrInvalidDeleteFlag)
	}
	// deleted flag already read
	offset := uint16(1)
	for i := 0; i < int(file.ColumnsCount()); i++ {
		column := file.table.columns[i]
		raw := data[offset : offset+uint16(column.Length)]
		val, err := file.interpretLocked(raw, file.table.columns[i], rec.Position)
		if err != nil {
			return nil, WrapError(err)
		}
		if file.config.TrimSpaces {
			if str, ok := val.(string); ok {
				val = strings.TrimSpace(str)
			}

			if bslice, ok := val.([]byte); ok {
				val = sanitizeEmptyBytes(bslice)
			}
		}
		if file.config.CollapseSpaces {
			if str, ok := val.(string); ok {
				val = sanitizeSpaces(str)
			}
		}
		field := &Field{
			column: column,
			value:  val,
		}

		if DataType(column.DataType) == Memo {
			field.memoPos = raw
		}

		rec.fields = append(rec.fields, field)
		offset += uint16(column.Length)
	}
	return rec, nil
}

// Converts a map of interfaces into the row representation
func (file *File) RowFromMap(m map[string]interface{}) (*Row, error) {
	// Building the row may run autoincrement, which rewrites the column
	// descriptors, so it takes the write lock.
	end, err := file.beginWrite()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	return file.rowFromMapLocked(m)
}

func (file *File) rowFromMapLocked(m map[string]interface{}) (*Row, error) {
	debugf("Converting map to row... \n%+v", m)
	row := file.newRowLocked()
	for i := range row.fields {
		field := &Field{column: file.table.columns[i]}
		if val, ok := m[field.Name()]; ok {
			field.value = val
		}

		if i >= 0 && i < len(file.table.mods) {
			if mod := file.table.mods[i]; mod != nil {
				if len(mod.ExternalKey) != 0 {
					if val, ok := m[mod.ExternalKey]; ok {
						debugf("Resolving external key %v for field %v due to modification", mod.ExternalKey, field.Name())
						field.value = val
					}
				}

				if mod.Convert != nil {
					debugf("Converting field %v due to modification", field.Name())
					var err error
					field.value, err = mod.Convert(field.value)
					if err != nil {
						return nil, WrapError(err)
					}
				}
			}
		}

		row.fields[i] = field
	}
	if err := row.incrementLocked(); err != nil {
		return nil, WrapError(err)
	}
	return row, nil
}

// Converts a JSON-encoded row into the row representation
func (file *File) RowFromJSON(j []byte) (*Row, error) {
	end, err := file.beginWrite()
	if err != nil {
		return nil, WrapError(err)
	}
	defer end()
	debugf("Converting JSON to row...")
	m := make(map[string]interface{})
	err = json.Unmarshal(j, &m)
	if err != nil {
		return nil, NewError("unable to unmarshal JSON").Details(err)
	}
	row, err := file.rowFromMapLocked(m)
	if err != nil {
		return nil, WrapError(err)
	}
	return row, nil
}

// Converts a struct into the row representation
// The struct must have the same field names as the columns in the table or the dbase tag must be set.
// The dbase tag can be used to name the field. For example: `dbase:"my_field_name"`
func (file *File) RowFromStruct(v interface{}) (*Row, error) {
	debugf("Converting struct to row...")
	m := make(map[string]interface{})
	rt := reflect.TypeOf(v)
	if rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		tag := field.Tag.Get("dbase")
		if len(tag) == 0 {
			tag = field.Name
		}
		m[tag] = rv.Field(i).Interface()
	}
	row, err := file.rowFromMapLocked(m)
	if err != nil {
		return nil, WrapError(err)
	}
	return row, nil
}

// columnLocked returns the column at pos while the lifecycle lock is held.
func (file *File) columnLocked(pos int) *Column {
	if pos < 0 || pos >= len(file.table.columns) {
		return nil
	}
	return file.table.columns[pos]
}
