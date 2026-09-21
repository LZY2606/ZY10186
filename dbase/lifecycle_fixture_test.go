package dbase

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// lifecycleColumns is the smallest table that still exercises every column
// family mentioned in the record lifecycle: text, numeric, date, logical,
// empty values and a memo stored in the FPT.
func lifecycleColumns(t *testing.T) []*Column {
	t.Helper()
	specs := []struct {
		name     string
		typ      DataType
		length   uint8
		decimals uint8
	}{
		{"NAME", Character, 10, 0},
		{"NUM", Numeric, 5, 0},
		{"BIRTH", Date, 0, 0},
		{"ACTIVE", Logical, 0, 0},
		{"NOTE", Memo, 0, 0},
	}
	columns := make([]*Column, 0, len(specs))
	for _, spec := range specs {
		column, err := NewColumn(spec.name, spec.typ, spec.length, spec.decimals, false)
		if err != nil {
			t.Fatalf("column %s: %v", spec.name, err)
		}
		columns = append(columns, column)
	}
	return columns
}

// lifecycleStore is an in-memory DBF/FPT pair backed by GenericIO. Using
// memory keeps the byte assertions exact, avoids filesystem case handling and
// removes all dependence on directory iteration or real file permissions.
type lifecycleStore struct {
	dbf *BytesReadWriteSeeker
	fpt *BytesReadWriteSeeker
}

func newLifecycleStore() *lifecycleStore {
	return &lifecycleStore{
		dbf: NewBytesReadWriteSeeker([]byte{}),
		fpt: NewBytesReadWriteSeeker([]byte{}),
	}
}

func (store *lifecycleStore) config() *Config {
	return &Config{
		// Filename is only used to derive the table name; IO provides the
		// actual data source, so it must not also be set when opening.
		Filename:  "LIFECYCLE.DBF",
		Converter: NewDefaultConverter(charmap.Windows1252),
	}
}

func (store *lifecycleStore) create(t *testing.T) *File {
	t.Helper()
	cfg := store.config()
	generic := GenericIO{Handle: store.dbf, RelatedHandle: store.fpt}
	file, err := NewTable(FoxPro, cfg, lifecycleColumns(t), 64, generic)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return file
}

func (store *lifecycleStore) open(t *testing.T) *File {
	t.Helper()
	cfg := store.config()
	cfg.Filename = ""
	cfg.IO = GenericIO{Handle: store.dbf, RelatedHandle: store.fpt}
	file, err := OpenTable(cfg)
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}
	return file
}

func (store *lifecycleStore) dbfBytes() []byte { return store.dbf.Data() }
func (store *lifecycleStore) fptBytes() []byte { return store.fpt.Data() }

const (
	lifecycleFirstRow = uint16(296 + 5*32) // 456
	lifecycleRowLen   = uint16(1 + 10 + 5 + 8 + 1 + 4)
	lifecycleBlock    = 64
)

func lifecycleHeaderFields(t *testing.T, raw []byte) Header {
	t.Helper()
	if len(raw) < 32 {
		t.Fatalf("DBF shorter than 32 byte header: %d", len(raw))
	}
	var header Header
	if err := binary.Read(bytes.NewReader(raw[:30]), binary.LittleEndian, &header); err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	return header
}

// assertHeader checks the stable header fields. The last-update date is the
// only intentionally clock driven field and is skipped on purpose so the test
// never depends on the real clock.
func assertHeader(t *testing.T, header Header, rows uint32) {
	t.Helper()
	if header.FileType != byte(FoxPro) {
		t.Errorf("FileType = 0x%02x, want 0x%02x", header.FileType, FoxPro)
	}
	if header.RowsCount != rows {
		t.Errorf("RowsCount = %d, want %d", header.RowsCount, rows)
	}
	if header.FirstRow != lifecycleFirstRow {
		t.Errorf("FirstRow = %d, want %d", header.FirstRow, lifecycleFirstRow)
	}
	if header.RowLength != lifecycleRowLen {
		t.Errorf("RowLength = %d, want %d", header.RowLength, lifecycleRowLen)
	}
	if header.CodePage != 0x03 {
		t.Errorf("CodePage = 0x%02x, want 0x03 (Windows-1252)", header.CodePage)
	}
	if header.TableFlags != byte(MemoFlag) {
		t.Errorf("TableFlags = 0x%02x, want memo flag 0x%02x", header.TableFlags, MemoFlag)
	}
}

// TestRecordLifecycleByteFixture walks one record through create, reopen,
// update, soft delete and reopen again while asserting header counts, record
// offsets and the FPT references byte by byte.
func TestRecordLifecycleByteFixture(t *testing.T) {
	store := newLifecycleStore()
	file := store.create(t)

	if file.Header().RowsCount != 0 {
		t.Fatalf("new table RowsCount = %d, want 0", file.Header().RowsCount)
	}

	birth := time.Date(1990, time.March, 15, 0, 0, 0, 0, time.UTC)
	row, err := file.RowFromMap(map[string]interface{}{
		"NAME": "Alice", "NUM": int64(42), "BIRTH": birth.Format(time.RFC3339),
		"ACTIVE": true, "NOTE": "hello memo",
	})
	if err != nil {
		t.Fatalf("RowFromMap: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if file.RowsCount() != 1 {
		t.Fatalf("RowsCount after add = %d, want 1", file.RowsCount())
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Phase 1: bytes of the freshly created table.
	dbf := store.dbfBytes()
	fpt := store.fptBytes()
	assertHeader(t, lifecycleHeaderFields(t, dbf), 1)

	// Column descriptors live at offsets 32..32+n*32, each terminated by 0x0D.
	wantNames := []string{"NAME", "NUM", "BIRTH", "ACTIVE", "NOTE"}
	for i, name := range wantNames {
		off := 32 + i*32
		if string(bytes.TrimRight(dbf[off:off+11], "\x00")) != name {
			t.Errorf("column %d name bytes = %q, want %q", i, dbf[off:off+11], name)
		}
	}
	termOff := 32 + 5*32
	if dbf[termOff] != byte(ColumnEnd) {
		t.Errorf("header terminator at %d = 0x%02x, want 0x%02x", termOff, dbf[termOff], ColumnEnd)
	}

	// Record 0 starts exactly at FirstRow.
	recOff := int(lifecycleFirstRow)
	record0 := dbf[recOff : recOff+int(lifecycleRowLen)]
	wantRecord := []byte(" " + "Alice     " + "   42" + "19900315" + "T" + "\x08\x00\x00\x00")
	if !bytes.Equal(record0, wantRecord) {
		t.Errorf("record0 bytes =\n  % x\nwant\n  % x", record0, wantRecord)
	}

	// FPT: 512 byte header, block size 64, first free block 8, data at block 8.
	if len(fpt) != 512+64 {
		t.Errorf("FPT size = %d, want %d", len(fpt), 512+64)
	}
	if got := binary.BigEndian.Uint32(fpt[0:4]); got != 9 {
		t.Errorf("FPT NextFree = %d, want 9", got)
	}
	if got := binary.BigEndian.Uint16(fpt[6:8]); got != 64 {
		t.Errorf("FPT BlockSize = %d, want 64", got)
	}
	memoOff := 8 * 64
	if got := binary.BigEndian.Uint32(fpt[memoOff : memoOff+4]); got != 1 {
		t.Errorf("memo signature = %d, want 1 (text)", got)
	}
	if got := binary.BigEndian.Uint32(fpt[memoOff+4 : memoOff+8]); got != 10 {
		t.Errorf("memo length = %d, want 10", got)
	}
	if got := string(bytes.TrimRight(fpt[memoOff+8:memoOff+18], "\x00")); got != "hello memo" {
		t.Errorf("memo payload = %q, want %q", got, "hello memo")
	}

	// Phase 2: reopen and verify decoded values.
	file = store.open(t)
	assertHeader(t, *file.Header(), 1)
	reread, err := file.ReadRowAt(0)
	if err != nil {
		t.Fatalf("ReadRowAt after reopen: %v", err)
	}
	if reread.ByteOffset != 0 {
		t.Errorf("ByteOffset = %d, want 0", reread.ByteOffset)
	}
	values := reread.Values()
	if got := fmt.Sprintf("%v", values[0]); got != "Alice     " {
		t.Errorf("NAME = %q, want padded Alice", values[0])
	}
	if got, ok := values[1].(int64); !ok || got != 42 {
		t.Errorf("NUM = %v (%T), want int64 42", values[1], values[1])
	}
	if got, ok := values[2].(time.Time); !ok || !got.Equal(birth) {
		t.Errorf("BIRTH = %v, want %v", values[2], birth)
	}
	if got, ok := values[3].(bool); !ok || !got {
		t.Errorf("ACTIVE = %v, want true", values[3])
	}
	if got, ok := values[4].(string); !ok || got != "hello memo" {
		t.Errorf("NOTE = %v (%T), want hello memo", values[4], values[4])
	}

	// Phase 3: update text and memo in place; offsets and counts must not move.
	if err := reread.Field(0).SetValue("Bob"); err != nil {
		t.Fatalf("SetValue NAME: %v", err)
	}
	if err := reread.Field(1).SetValue(int64(7)); err != nil {
		t.Fatalf("SetValue NUM: %v", err)
	}
	if err := reread.Field(4).SetValue("hello memo updated"); err != nil {
		t.Fatalf("SetValue NOTE: %v", err)
	}
	if err := reread.Write(); err != nil {
		t.Fatalf("Write update: %v", err)
	}
	if file.RowsCount() != 1 {
		t.Errorf("RowsCount after in-place update = %d, want 1", file.RowsCount())
	}
	dbf = store.dbfBytes()
	fpt = store.fptBytes()
	record0 = dbf[recOff : recOff+int(lifecycleRowLen)]
	wantRecord = []byte(" " + "Bob       " + "    7" + "19900315" + "T" + "\x08\x00\x00\x00")
	if !bytes.Equal(record0, wantRecord) {
		t.Errorf("updated record0 bytes =\n  % x\nwant\n  % x", record0, wantRecord)
	}
	if binary.BigEndian.Uint32(fpt[0:4]) != 9 {
		t.Errorf("FPT NextFree changed after in-place memo rewrite to %d, want 9",
			binary.BigEndian.Uint32(fpt[0:4]))
	}
	if got := string(bytes.TrimRight(fpt[memoOff+8:memoOff+26], "\x00")); got != "hello memo updated" {
		t.Errorf("updated memo payload = %q, want %q", got, "hello memo updated")
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close after update: %v", err)
	}

	// Phase 4: append a second row; new memo goes to block 9, record at +29.
	file = store.open(t)
	second, err := file.RowFromMap(map[string]interface{}{
		"NAME": "Carol", "NUM": int64(100), "BIRTH": "", "ACTIVE": false, "NOTE": "second",
	})
	if err != nil {
		t.Fatalf("RowFromMap second: %v", err)
	}
	if err := second.Add(); err != nil {
		t.Fatalf("Add second: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close after append: %v", err)
	}
	dbf = store.dbfBytes()
	fpt = store.fptBytes()
	assertHeader(t, lifecycleHeaderFields(t, dbf), 2)
	if len(dbf) != int(lifecycleFirstRow)+2*int(lifecycleRowLen) {
		t.Errorf("DBF size = %d, want %d", len(dbf), int(lifecycleFirstRow)+2*int(lifecycleRowLen))
	}
	rec1 := dbf[recOff+int(lifecycleRowLen):][:int(lifecycleRowLen)]
	wantRecord1 := []byte(" " + "Carol     " + "  100" + "        " + "F" + "\x09\x00\x00\x00")
	if !bytes.Equal(rec1, wantRecord1) {
		t.Errorf("record1 bytes =\n  % x\nwant\n  % x", rec1, wantRecord1)
	}
	if binary.BigEndian.Uint32(fpt[0:4]) != 10 {
		t.Errorf("FPT NextFree = %d, want 10", binary.BigEndian.Uint32(fpt[0:4]))
	}
	memo1Off := 9 * 64
	if got := string(bytes.TrimRight(fpt[memo1Off+8:memo1Off+14], "\x00")); got != "second" {
		t.Errorf("second memo payload = %q, want second", got)
	}

	// Phase 5: soft delete only flips the marker. Count, offsets and FPT bytes
	// are untouched because the library has no physical PACK operation.
	file = store.open(t)
	if err := file.GoTo(0); err != nil {
		t.Fatalf("GoTo(0): %v", err)
	}
	deleted0, err := file.Deleted()
	if err != nil {
		t.Fatalf("Deleted before: %v", err)
	}
	if deleted0 {
		t.Fatal("record 0 reported deleted before marking")
	}
	target, err := file.Row()
	if err != nil {
		t.Fatalf("Row to delete: %v", err)
	}
	target.Deleted = true
	if err := target.Write(); err != nil {
		t.Fatalf("Write delete marker: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close after delete: %v", err)
	}
	dbf = store.dbfBytes()
	assertHeader(t, lifecycleHeaderFields(t, dbf), 2)
	if dbf[recOff] != byte(Deleted) {
		t.Errorf("delete marker = 0x%02x, want 0x%02x", dbf[recOff], Deleted)
	}
	if dbf[recOff+int(lifecycleRowLen)] != byte(Active) {
		t.Errorf("record1 marker = 0x%02x, want 0x%02x", dbf[recOff+int(lifecycleRowLen)], Active)
	}
	if !bytes.Equal(fpt, store.fptBytes()) {
		t.Error("soft delete changed FPT bytes")
	}

	// Phase 6: reopen distinguishes marker from physical removal.
	file = store.open(t)
	all, err := file.Rows(false, false)
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("Rows count = %d, want 2 (soft delete keeps the record)", len(all))
	}
	if !all[0].Deleted || all[1].Deleted {
		t.Errorf("deleted flags = %v/%v, want true/false", all[0].Deleted, all[1].Deleted)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("final Close: %v", err)
	}
}
