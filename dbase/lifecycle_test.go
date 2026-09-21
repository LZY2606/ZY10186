package dbase

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// lifecycleColumns defines a table covering text, numeric, date, logical,
// nullable varchar and memo columns.
func lifecycleColumns(t *testing.T) []*Column {
	t.Helper()
	specs := []struct {
		name     string
		dataType DataType
		length   uint8
		decimals uint8
		nullable bool
	}{
		{"NAME", Character, 10, 0, false},
		{"AMOUNT", Numeric, 8, 2, false},
		{"WHEN", Date, 8, 0, false},
		{"ACTIVE", Logical, 1, 0, false},
		{"LABEL", Varchar, 12, 0, true},
		{"NOTE", Memo, 4, 0, false},
	}
	columns := make([]*Column, 0, len(specs)+1)
	for _, spec := range specs {
		column, err := NewColumn(spec.name, spec.dataType, spec.length, spec.decimals, spec.nullable)
		if err != nil {
			t.Fatalf("creating column %s: %v", spec.name, err)
		}
		columns = append(columns, column)
	}
	return columns
}

// lifecycleStore is an in-memory DBF/FPT pair created through GenericIO.
type lifecycleStore struct {
	dbf *BytesReadWriteSeeker
	fpt *BytesReadWriteSeeker
}

func newLifecycleStore(t *testing.T) (*File, *lifecycleStore) {
	t.Helper()
	store := &lifecycleStore{
		dbf: NewBytesReadWriteSeeker([]byte{}),
		fpt: NewBytesReadWriteSeeker([]byte{}),
	}
	file, err := NewTable(FoxPro, &Config{
		Filename:  "LIFE.DBF",
		Converter: NewDefaultConverter(charmap.Windows1252),
	}, lifecycleColumns(t), 64, GenericIO{Handle: store.dbf, RelatedHandle: store.fpt})
	if err != nil {
		t.Fatalf("creating lifecycle table: %v", err)
	}
	return file, store
}

func reopenLifecycle(t *testing.T, store *lifecycleStore) *File {
	t.Helper()
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: store.dbf, RelatedHandle: store.fpt},
	})
	if err != nil {
		t.Fatalf("reopening lifecycle table: %v", err)
	}
	return file
}

func (store *lifecycleStore) dbfBytes() []byte { return store.dbf.Data() }
func (store *lifecycleStore) fptBytes() []byte { return store.fpt.Data() }

const (
	lifeRowLength    = 45          // delete flag 1 + NAME 10 + AMOUNT 8 + WHEN 8 + ACTIVE 1 + LABEL 12 + NOTE 4 + _NullFlags 1
	lifeFirstRow     = uint16(520) // 32 + 7*32 field descriptors (incl. _NullFlags) + 0x0d + 216 reserved
	lifeMemoBlockSz  = 64
	lifeFirstMemoBlk = 8 // 512 byte FPT header / 64
)

// assertDbfLayout verifies the immutable structural bytes after creation.
func assertDbfLayout(t *testing.T, file *File, raw []byte) {
	t.Helper()
	if got := file.Header().RowLength; got != lifeRowLength {
		t.Fatalf("header row length = %d, want %d", got, lifeRowLength)
	}
	if got := file.Header().FirstRow; got != lifeFirstRow {
		t.Fatalf("header first row offset = %d, want %d", got, lifeFirstRow)
	}
	if raw[0] != byte(FoxPro) {
		t.Errorf("file type byte = 0x%02x, want 0x%02x", raw[0], byte(FoxPro))
	}
	if raw[28] != byte(MemoFlag) {
		t.Errorf("table flags = 0x%02x, want memo flag 0x%02x", raw[28], byte(MemoFlag))
	}
	if raw[29] != file.config.Converter.CodePage() {
		t.Errorf("code page mark = 0x%02x, want 0x%02x", raw[29], file.config.Converter.CodePage())
	}
	termOffset := int(32 + 7*32)
	if raw[termOffset] != byte(ColumnEnd) {
		t.Errorf("header terminator at %d = 0x%02x, want 0x%02x",
			termOffset, raw[termOffset], byte(ColumnEnd))
	}
}

func TestRecordLifecycleByteFixtures(t *testing.T) {
	file, store := newLifecycleStore(t)

	birth := time.Date(2026, time.May, 17, 0, 0, 0, 0, time.UTC)
	values := map[string]interface{}{
		"NAME":   "Göteborg", // 1252 encodes ö (0xF6) as a single byte
		"AMOUNT": 42.50,
		"WHEN":   birth,
		"ACTIVE": true,
		"LABEL":  nil, // nullable varchar, null
		"NOTE":   "memo-one",
	}
	row, err := file.RowFromMap(values)
	if err != nil {
		t.Fatalf("building row: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("adding row: %v", err)
	}
	if got := file.RowsCount(); got != 1 {
		t.Fatalf("RowsCount after add = %d, want 1", got)
	}

	dbf := store.dbfBytes()
	fpt := store.fptBytes()
	assertDbfLayout(t, file, dbf)

	// Header record count (LE uint32 at offset 4) must match the single row.
	if got := binary.LittleEndian.Uint32(dbf[4:8]); got != 1 {
		t.Fatalf("header RowsCount bytes = %d, want 1", got)
	}
	if len(dbf) < int(lifeFirstRow)+lifeRowLength {
		t.Fatalf("dbf size %d smaller than header+one row %d", len(dbf), int(lifeFirstRow)+lifeRowLength)
	}

	// --- Record slot bytes at offset FirstRow ---
	rec := dbf[lifeFirstRow : lifeFirstRow+lifeRowLength]
	if rec[0] != byte(Active) {
		t.Errorf("new record delete flag = 0x%02x, want 0x20", rec[0])
	}
	nameRaw := rec[1:11]
	if want := []byte{'G', 0xF6, 't', 'e', 'b', 'o', 'r', 'g', ' ', ' '}; !bytes.Equal(nameRaw, want) {
		t.Errorf("NAME field bytes = % x, want % x", nameRaw, want)
	}
	amountRaw := rec[11:19]
	if want := []byte("   42.50"); !bytes.Equal(amountRaw, want) {
		t.Errorf("AMOUNT field bytes = %q, want %q", amountRaw, want)
	}
	whenRaw := rec[19:27]
	if want := []byte("20260517"); !bytes.Equal(whenRaw, want) {
		t.Errorf("WHEN field bytes = %q, want %q", whenRaw, want)
	}
	if rec[27] != 'T' {
		t.Errorf("ACTIVE field = %q, want T", rec[27])
	}
	labelRaw := rec[28:40]
	if !isEmptyBytes(labelRaw) {
		t.Errorf("nil LABEL field = % x, want all zero", labelRaw)
	}
	notePointer := rec[40:44]
	if got := binary.LittleEndian.Uint32(notePointer); got != lifeFirstMemoBlk {
		t.Fatalf("memo pointer = %d, want first data block %d", got, lifeFirstMemoBlk)
	}
	// Null flag trailing byte: null bit (bit 1) set for the nullable varchar.
	if rec[44]&0x02 != 0x02 {
		t.Errorf("_NullFlags = %08b, want null bit set (bit1)", rec[44])
	}

	// --- FPT layout: 512 byte header, first entry at block 8 ---
	if len(fpt) != 576 {
		t.Fatalf("fpt size = %d, want 576 (512 header + one 64 byte block)", len(fpt))
	}
	if got := binary.BigEndian.Uint32(fpt[0:4]); got != lifeFirstMemoBlk+1 {
		t.Errorf("fpt NextFree = %d, want %d", got, lifeFirstMemoBlk+1)
	}
	if got := binary.BigEndian.Uint16(fpt[6:8]); got != lifeMemoBlockSz {
		t.Errorf("fpt BlockSize = %d, want %d", got, lifeMemoBlockSz)
	}
	entry := fpt[512:]
	if got := binary.BigEndian.Uint32(entry[0:4]); got != 1 {
		t.Errorf("memo signature = %d, want 1 (text)", got)
	}
	if got := binary.BigEndian.Uint32(entry[4:8]); got != uint32(len("memo-one")) {
		t.Errorf("memo length = %d, want %d", got, len("memo-one"))
	}
	if got := bytes.TrimRight(entry[8:], "\x00"); string(got) != "memo-one" {
		t.Errorf("memo payload = %q, want memo-one", got)
	}

	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// --- Reopen and interpret the same bytes ---
	reopened := reopenLifecycle(t, store)
	defer reopened.Close()
	gotRow, err := reopened.Row()
	if err != nil {
		t.Fatalf("reading reopened row: %v", err)
	}
	// Character values are fixed width and space padded; TrimSpaces is a
	// config option, so the reopened raw value carries the two pad spaces.
	if name, err := gotRow.StringValueByName("NAME"); err != nil || name != "Göteborg  " {
		t.Errorf("NAME = %q, %v; want Göteborg padded to 10 bytes", name, err)
	}
	if amount, err := gotRow.FloatValueByName("AMOUNT"); err != nil || amount != 42.50 {
		t.Errorf("AMOUNT = %v, %v; want 42.5", amount, err)
	}
	if when, err := gotRow.TimeValueByName("WHEN"); err != nil || !when.Equal(birth) {
		t.Errorf("WHEN = %v, %v; want %v", when, err, birth)
	}
	if active, err := gotRow.BoolValueByName("ACTIVE"); err != nil || !active {
		t.Errorf("ACTIVE = %v, %v; want true", active, err)
	}
	if label := gotRow.FieldByName("LABEL").GetValue(); label != nil && !bytes.Equal(toBytes(label), []byte{}) {
		t.Errorf("LABEL = %#v, want nil/empty", label)
	}
	if note, err := gotRow.StringValueByName("NOTE"); err != nil || note != "memo-one" {
		t.Errorf("NOTE = %q, %v; want memo-one", note, err)
	}
}

func toBytes(v interface{}) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	return nil
}

func TestRecordLifecycleUpdateAndDelete(t *testing.T) {
	file, store := newLifecycleStore(t)
	row, err := file.RowFromMap(map[string]interface{}{
		"NAME":   "Alice",
		"AMOUNT": 1.0,
		"WHEN":   time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC),
		"ACTIVE": false,
		"LABEL":  "lbl",
		"NOTE":   "first-note",
	})
	if err != nil {
		t.Fatalf("building row: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("adding row: %v", err)
	}
	firstOffset := int64(file.Header().FirstRow)
	if row.ByteOffset != 0 && row.ByteOffset != firstOffset {
		t.Errorf("ByteOffset = %d, want %d", row.ByteOffset, firstOffset)
	}

	// Update the existing row: memo rewritten in place at the same block,
	// label stays a short varchar (length marker + varlength bit).
	row.Position = 0 // Row.Position is 1-based on disk: writing index 0 requires resetting from Add's append slot
	row.Fields()[0].SetValue("Bob")
	row.Fields()[5].SetValue("second-note")
	if err := row.Write(); err != nil {
		t.Fatalf("updating row: %v", err)
	}
	if got := file.RowsCount(); got != 1 {
		t.Fatalf("RowsCount after update = %d, want 1 (update must not append)", got)
	}
	dbf := store.dbfBytes()
	rec := dbf[lifeFirstRow : lifeFirstRow+lifeRowLength]
	if !bytes.Equal(rec[1:11], []byte{'B', 'o', 'b', ' ', ' ', ' ', ' ', ' ', ' ', ' '}) {
		t.Errorf("updated NAME = % x", rec[1:11])
	}
	pointer := binary.LittleEndian.Uint32(rec[40:44])
	if pointer != lifeFirstMemoBlk+1 {
		t.Errorf("memo pointer after update = %d, want newly appended block %d", pointer, lifeFirstMemoBlk+1)
	}
	// Short varchar: last field byte holds the length (3), varlength bit 0 set.
	if rec[39] != 3 {
		t.Errorf("LABEL length marker = %d, want 3", rec[39])
	}
	if rec[44]&0x01 != 0x01 {
		t.Errorf("_NullFlags = %08b, want varlength bit0 set", rec[44])
	}
	if rec[44]&0x02 != 0 {
		t.Errorf("_NullFlags = %08b, null bit must stay clear for non-null varchar", rec[44])
	}
	// In-place rewrite: the DBF pointer keeps block 8, but the library appends
	// the new entry and advances NextFree (old block becomes unreferenced).
	fpt := store.fptBytes()
	secondEntry := fpt[9*lifeMemoBlockSz:]
	if got := binary.BigEndian.Uint32(secondEntry[4:8]); got != uint32(len("second-note")) {
		t.Errorf("updated memo length = %d, want %d", got, len("second-note"))
	}
	if got := binary.BigEndian.Uint32(fpt[0:4]); got != lifeFirstMemoBlk+2 {
		t.Errorf("fpt NextFree = %d, want %d", got, lifeFirstMemoBlk+2)
	}

	// Appending a second row allocates a fresh memo block.
	row2, err := file.RowFromMap(map[string]interface{}{
		"NAME":   "Carol",
		"AMOUNT": 2.0,
		"WHEN":   time.Date(2026, time.March, 3, 0, 0, 0, 0, time.UTC),
		"ACTIVE": true,
		"LABEL":  nil,
		"NOTE":   "third-note",
	})
	if err != nil {
		t.Fatalf("building second row: %v", err)
	}
	if err := row2.Add(); err != nil {
		t.Fatalf("adding second row: %v", err)
	}
	if got := file.RowsCount(); got != 2 {
		t.Fatalf("RowsCount after second add = %d, want 2", got)
	}
	dbf = store.dbfBytes()
	rec2 := dbf[lifeFirstRow+lifeRowLength : lifeFirstRow+2*lifeRowLength]
	secondPointer := binary.LittleEndian.Uint32(rec2[40:44])
	if secondPointer != lifeFirstMemoBlk+2 {
		t.Errorf("second memo pointer = %d, want %d", secondPointer, lifeFirstMemoBlk+2)
	}
	if rec2[0] != byte(Active) {
		t.Errorf("second row marker = 0x%02x, want active", rec2[0])
	}

	// Logical delete: only the marker byte and a rewrite change; count and
	// record bytes are otherwise untouched. Reopen proves persistence.
	if err := file.GoTo(0); err != nil {
		t.Fatalf("goto 0: %v", err)
	}
	reread, err := file.Row()
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	reread.Deleted = true
	if err := reread.Write(); err != nil {
		t.Fatalf("writing delete marker: %v", err)
	}
	if got := file.RowsCount(); got != 2 {
		t.Errorf("RowsCount after delete = %d, want 2 (logical delete keeps count)", got)
	}
	dbf = store.dbfBytes()
	if dbf[lifeFirstRow] != byte(Deleted) {
		t.Errorf("first record marker = 0x%02x, want 0x2a deleted", dbf[lifeFirstRow])
	}
	if dbf[lifeFirstRow+lifeRowLength] != byte(Active) {
		t.Errorf("second record marker = 0x%02x, want still active", dbf[lifeFirstRow+lifeRowLength])
	}
	if got := binary.LittleEndian.Uint32(dbf[4:8]); got != 2 {
		t.Errorf("header RowsCount = %d, want 2 after logical delete", got)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := reopenLifecycle(t, store)
	defer reopened.Close()
	deletedNow, err := reopened.Deleted()
	if err != nil {
		t.Fatalf("Deleted at row 0: %v", err)
	}
	if !deletedNow {
		t.Errorf("row 0 deleted flag = false after reopen, want true")
	}
	all, err := reopened.Rows(false, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("Rows = %d rows, %v; want both physical records still present", len(all), err)
	}
	live, err := reopened.Rows(true, true)
	if err != nil || len(live) != 1 {
		t.Fatalf("Rows(skipDeleted) = %d rows, %v; want 1", len(live), err)
	}
	if name, _ := live[0].StringValueByName("NAME"); name != "Carol     " {
		t.Errorf("surviving row NAME = %q, want Carol padded", name)
	}

	// The marker is separate from physical removal: bytes of the deleted
	// record remain readable directly through the raw row API.
	raw0, err := reopened.ReadRow(0)
	if err != nil {
		t.Fatalf("raw read of deleted row: %v", err)
	}
	if raw0[0] != byte(Deleted) {
		t.Errorf("raw deleted marker = 0x%02x, want 0x2a", raw0[0])
	}
}

// failingDecoder rejects input that is not already valid UTF-8, simulating a
// charset conversion failure without depending on external encodings.
type failingDecoder struct{}

func (failingDecoder) Decode(in []byte) ([]byte, error) {
	for _, b := range in {
		if b >= 0x80 {
			return nil, errors.New("simulated charset decode failure")
		}
	}
	return in, nil
}
func (failingDecoder) Encode(in []byte) ([]byte, error) { return in, nil }
func (failingDecoder) CodePage() byte                   { return 0x03 }

func TestDiagnosticCharsetFailure(t *testing.T) {
	dbf := NewBytesReadWriteSeeker([]byte{})
	fpt := NewBytesReadWriteSeeker([]byte{})
	columns := []*Column{mustColumn(t, "NAME", Character, 10, 0, false)}
	file, err := NewTable(FoxPro, &Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: dbf, RelatedHandle: fpt},
	}, columns, 0, GenericIO{Handle: dbf, RelatedHandle: fpt})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Write raw 0x81 through a struct-less row: bypass encode by planting bytes.
	row := file.NewRow()
	if err := row.Field(0).SetValue("placeholder"); err != nil {
		t.Fatal(err)
	}
	if err := row.Add(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw := dbf.Data()
	raw[file.Header().FirstRow+1] = 0x81 // invalid under the failing decoder

	bad, err := OpenTable(&Config{
		Converter: failingDecoder{},
		IO:        GenericIO{Handle: NewBytesReadWriteSeeker(raw)},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer bad.Close()
	_, err = bad.Row()
	if err == nil {
		t.Fatal("expected charset error, got nil")
	}
	if !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("error %v should wrap ErrInvalidEncoding", err)
	}
}

func mustColumn(t *testing.T, name string, dt DataType, length, decimals uint8, nullable bool) *Column {
	t.Helper()
	c, err := NewColumn(name, dt, length, decimals, nullable)
	if err != nil {
		t.Fatalf("column %s: %v", name, err)
	}
	return c
}

// buildSingleRowTable creates a minimal table with one active row and returns
// its raw bytes for tampering based diagnostics.
func buildSingleRowTable(t *testing.T, columns []*Column, values map[string]interface{}) (dbfData, fptData []byte, firstRow uint16, rowLength uint16) {
	t.Helper()
	dbf := NewBytesReadWriteSeeker([]byte{})
	fpt := NewBytesReadWriteSeeker([]byte{})
	hasMemo := false
	for _, c := range columns {
		if c.DataType == byte(Memo) {
			hasMemo = true
		}
	}
	ioImpl := GenericIO{Handle: dbf, RelatedHandle: fpt}
	file, err := NewTable(FoxPro, &Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        ioImpl,
	}, columns, 64, ioImpl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	row, err := file.RowFromMap(values)
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !hasMemo {
		return dbf.Data(), nil, file.Header().FirstRow, file.Header().RowLength
	}
	return dbf.Data(), fpt.Data(), file.Header().FirstRow, file.Header().RowLength
}

func TestDiagnosticTruncatedRecord(t *testing.T) {
	columns := []*Column{mustColumn(t, "NAME", Character, 10, 0, false)}
	dbfData, _, firstRow, rowLength := buildSingleRowTable(t, columns, map[string]interface{}{"NAME": "abc"})
	// Keep the declared file size (open-time validation passes) but make reads
	// inside the single record return three bytes short, simulating a damaged
	// record in an otherwise correctly sized file.
	short := &shortReadSeeker{
		data:      dbfData,
		shortFrom: int64(firstRow),
		shortBy:   3,
		recordLen: int64(rowLength),
	}
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: short},
	})
	if err != nil {
		t.Fatalf("open succeeds from header only, got %v", err)
	}
	defer file.Close()
	_, err = file.Row()
	if !errors.Is(err, ErrRowTruncated) {
		t.Fatalf("error %v should wrap ErrRowTruncated", err)
	}
	// Error must not move the cursor, leaving the table readable position.
	if file.Pointer() != 0 {
		t.Errorf("cursor after failed read = %d, want 0", file.Pointer())
	}
}

func TestDiagnosticHeaderDeclaresMoreRowsThanFile(t *testing.T) {
	columns := []*Column{mustColumn(t, "NAME", Character, 10, 0, false)}
	dbfData, _, _, _ := buildSingleRowTable(t, columns, map[string]interface{}{"NAME": "abc"})
	// Lie in the header: declare 5 records while only one fits on disk.
	binary.LittleEndian.PutUint32(dbfData[4:8], 5)
	_, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: NewBytesReadWriteSeeker(dbfData)},
	})
	if !errors.Is(err, ErrTableTruncated) {
		t.Fatalf("open error %v should wrap ErrTableTruncated", err)
	}
}

func TestDiagnosticInvalidDeleteMarker(t *testing.T) {
	columns := []*Column{mustColumn(t, "NAME", Character, 10, 0, false)}
	dbfData, _, firstRow, _ := buildSingleRowTable(t, columns, map[string]interface{}{"NAME": "abc"})
	dbfData[firstRow] = 0x51 // neither 0x20 nor 0x2a
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: NewBytesReadWriteSeeker(dbfData)},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer file.Close()
	_, err = file.Row()
	if !errors.Is(err, ErrInvalidMarker) {
		t.Fatalf("error %v should wrap ErrInvalidMarker", err)
	}
}

func TestDiagnosticMemoPointerFreeAndOutOfBounds(t *testing.T) {
	columns := []*Column{
		mustColumn(t, "NAME", Character, 10, 0, false),
		mustColumn(t, "NOTE", Memo, 4, 0, false),
	}
	dbfData, fptData, firstRow, _ := buildSingleRowTable(t, columns, map[string]interface{}{
		"NAME": "abc",
		"NOTE": "hello",
	})

	// Case 1: pointer references a block beyond NextFree (unwritten/free space).
	freeCase := append([]byte(nil), dbfData...)
	// Pad the FPT with zero blocks so the block is physically inside the file
	// but still beyond NextFree (which stays 9).
	paddedFpt := append(append([]byte(nil), fptData...), make([]byte, 20*64)...)
	binary.LittleEndian.PutUint32(freeCase[firstRow+11:firstRow+15], 11)
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO: GenericIO{
			Handle:        NewBytesReadWriteSeeker(freeCase),
			RelatedHandle: NewBytesReadWriteSeeker(paddedFpt),
		},
	})
	if err != nil {
		t.Fatalf("open free case: %v", err)
	}
	if _, err := file.Row(); !errors.Is(err, ErrMemoFree) {
		t.Fatalf("free block pointer error = %v, want ErrMemoFree", err)
	}
	file.Close()

	// Case 2: pointer inside the memo header (< first data block).
	headerCase := append([]byte(nil), dbfData...)
	binary.LittleEndian.PutUint32(headerCase[firstRow+11:firstRow+15], 2)
	file2, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO: GenericIO{
			Handle:        NewBytesReadWriteSeeker(headerCase),
			RelatedHandle: NewBytesReadWriteSeeker(fptData),
		},
	})
	if err != nil {
		t.Fatalf("open header case: %v", err)
	}
	if _, err := file2.Row(); !errors.Is(err, ErrMemoOutOfBounds) {
		t.Fatalf("header block pointer error = %v, want ErrMemoOutOfBounds", err)
	}
	file2.Close()

	// Case 3: pointer beyond the physical FPT end (truncated memo file).
	oobCase := append([]byte(nil), dbfData...)
	binary.LittleEndian.PutUint32(oobCase[firstRow+11:firstRow+15], 100)
	file3, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO: GenericIO{
			Handle:        NewBytesReadWriteSeeker(oobCase),
			RelatedHandle: NewBytesReadWriteSeeker(fptData),
		},
	})
	if err != nil {
		t.Fatalf("open oob case: %v", err)
	}
	defer file3.Close()
	if _, err := file3.Row(); !errors.Is(err, ErrMemoOutOfBounds) {
		t.Fatalf("oob pointer error = %v, want ErrMemoOutOfBounds", err)
	}
}

// shortReadSeeker wraps a byte slice and makes exactly one record region read
// short by shortBy bytes (returns fewer bytes, then io.EOF), modelling a
// damaged record without changing the declared file size.
type shortReadSeeker struct {
	data      []byte
	pos       int64
	shortFrom int64
	recordLen int64
	shortBy   int
	served    bool
}

func (s *shortReadSeeker) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.pos:])
	if !s.served && s.pos == s.shortFrom && int64(len(p)) == s.recordLen {
		n -= s.shortBy
		s.served = true
	}
	s.pos += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *shortReadSeeker) Write(p []byte) (int, error) {
	n := copy(s.data[s.pos:], p)
	s.pos += int64(n)
	return n, nil
}

func (s *shortReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	case io.SeekEnd:
		s.pos = int64(len(s.data)) + offset
	}
	return s.pos, nil
}

func (s *shortReadSeeker) Close() error { return nil }

// fanoutStore is a concurrency safe in-memory DBF backing store. Each goroutine
// gets an independent read cursor keyed by goroutine ID substitute (an atomic
// cursor token handed out on first Seek), so parallel readers never share a
// mutable position. Full-record reads and row writes park on channels, giving
// tests deterministic interleavings without sleeps.
type fanoutStore struct {
	mu       sync.Mutex
	data     []byte
	rowLen   int
	readPos  map[uint64]int64
	writePos map[uint64]int64
	nextID   uint64

	onReadStart  chan struct{}
	allowRead    chan struct{}
	onWriteStart chan struct{}
	allowWrite   chan struct{}
}

func newFanoutStore(seed []byte, rowLength int) *fanoutStore {
	return &fanoutStore{
		data:         append([]byte(nil), seed...),
		rowLen:       rowLength,
		readPos:      make(map[uint64]int64),
		writePos:     make(map[uint64]int64),
		onReadStart:  make(chan struct{}, 8),
		allowRead:    make(chan struct{}),
		onWriteStart: make(chan struct{}, 2),
		allowWrite:   make(chan struct{}),
	}
}

// token returns a stable cursor id for the calling goroutine.
var fanoutTokenMu sync.Mutex
var fanoutTokens = make(map[int64]uint64)
var fanoutTokenNext uint64

func (s *fanoutStore) token() uint64 {
	gid := goroutineID()
	fanoutTokenMu.Lock()
	defer fanoutTokenMu.Unlock()
	if id, ok := fanoutTokens[gid]; ok {
		return id
	}
	fanoutTokenNext++
	fanoutTokens[gid] = fanoutTokenNext
	return fanoutTokenNext
}

func (s *fanoutStore) Read(p []byte) (int, error) {
	id := s.token()
	if len(p) == s.rowLen {
		s.onReadStart <- struct{}{}
		<-s.allowRead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pos := s.readPos[id]
	if pos >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[pos:])
	s.readPos[id] = pos + int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *fanoutStore) Write(p []byte) (int, error) {
	id := s.token()
	if len(p) == s.rowLen {
		s.onWriteStart <- struct{}{}
		<-s.allowWrite
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pos := s.writePos[id]
	end := pos + int64(len(p))
	if end > int64(len(s.data)) {
		grown := make([]byte, end)
		copy(grown, s.data)
		s.data = grown
	}
	n := copy(s.data[pos:], p)
	s.writePos[id] = pos + int64(n)
	return n, nil
}

func (s *fanoutStore) Seek(offset int64, whence int) (int64, error) {
	id := s.token()
	s.mu.Lock()
	defer s.mu.Unlock()
	target := offset
	r := s.readPos[id]
	w := s.writePos[id]
	switch whence {
	case io.SeekCurrent:
		target = r + offset
		_ = w
	case io.SeekEnd:
		target = int64(len(s.data)) + offset
	}
	s.readPos[id] = target
	s.writePos[id] = target
	return target, nil
}

func (s *fanoutStore) Close() error { return nil }

// goroutineID extracts the runtime goroutine id for cursor isolation.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	stack := strings.TrimPrefix(string(buf[:n]), "goroutine ")
	idStr := strings.Fields(stack)[0]
	id, _ := strconv.ParseInt(idStr, 10, 64)
	return id
}

// seedConcurrencyTable builds and closes a one row table and returns the raw
// DBF/FPT bytes plus the declared row length.
func seedConcurrencyTable(t *testing.T) (dbfData, fptData []byte, rowLength uint16) {
	t.Helper()
	dbf := NewBytesReadWriteSeeker([]byte{})
	fpt := NewBytesReadWriteSeeker([]byte{})
	columns := []*Column{mustColumn(t, "NAME", Character, 10, 0, false)}
	impl := GenericIO{Handle: dbf, RelatedHandle: fpt}
	seed, err := NewTable(FoxPro, &Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        impl,
	}, columns, 0, impl)
	if err != nil {
		t.Fatal(err)
	}
	row, err := seed.RowFromMap(map[string]interface{}{"NAME": "first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := row.Add(); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	return dbf.Data(), fpt.Data(), seed.Header().RowLength
}

func TestConcurrencyBarrierTwoReadersOneWriter(t *testing.T) {
	dbfData, fptData, rowLength := seedConcurrencyTable(t)
	store := newFanoutStore(dbfData, int(rowLength))
	fpt := NewBytesReadWriteSeeker(fptData)
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: store, RelatedHandle: fpt},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	close(store.allowWrite) // writes do not need parking, only observation
	defer func() {
		select {
		case <-store.allowRead:
		default:
			close(store.allowRead)
		}
		_ = file.Close()
	}()

	if err := file.GoTo(0); err != nil {
		t.Fatal(err)
	}

	// Two readers both enter the parked record read before any writer can move.
	readerErrs := make(chan error, 2)
	readerVals := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, err := file.Row()
			if err != nil {
				readerErrs <- err
				return
			}
			v, _ := r.StringValueByName("NAME")
			readerVals <- v
		}()
	}
	for i := 0; i < 2; i++ {
		<-store.onReadStart
	}

	// Writer starts while readers are parked: the write lock excludes their
	// read locks, so it must neither reach the store nor finish.
	writerDone := make(chan error, 1)
	go func() {
		newRow := file.NewRow()
		_ = newRow.Field(0).SetValue("second")
		writerDone <- newRow.Add()
	}()
	select {
	case err := <-writerDone:
		t.Fatalf("writer finished while readers hold RLock: %v", err)
	case <-store.onWriteStart:
		t.Fatal("writer reached the store while readers were parked")
	default:
	}

	// Release readers: both see the stable pre-write value.
	close(store.allowRead)
	for i := 0; i < 2; i++ {
		select {
		case err := <-readerErrs:
			t.Fatalf("reader: %v", err)
		case v := <-readerVals:
			if v != "first     " {
				t.Errorf("reader saw %q, want 'first     '", v)
			}
		}
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if got := file.RowsCount(); got != 2 {
		t.Errorf("RowsCount = %d, want 2", got)
	}
}

func TestCloseInterleavedWithRead(t *testing.T) {
	dbfData, fptData, rowLength := seedConcurrencyTable(t)
	store := newFanoutStore(dbfData, int(rowLength))
	fpt := NewBytesReadWriteSeeker(fptData)
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: store, RelatedHandle: fpt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := file.GoTo(0); err != nil {
		t.Fatal(err)
	}

	readErr := make(chan error, 1)
	go func() {
		_, err := file.Row()
		readErr <- err
	}()
	<-store.onReadStart

	closeReturned := make(chan struct{})
	go func() {
		_ = file.Close()
		close(closeReturned)
	}()
	select {
	case <-closeReturned:
		t.Fatal("Close returned while a read was in flight")
	default:
	}

	close(store.allowRead)
	if err := <-readErr; err != nil {
		t.Fatalf("in-flight read should complete: %v", err)
	}
	<-closeReturned

	if _, err := file.Row(); !errors.Is(err, ErrClosed) {
		t.Errorf("Row after close = %v, want ErrClosed", err)
	}
	if _, err := file.Deleted(); !errors.Is(err, ErrClosed) {
		t.Errorf("Deleted after close = %v, want ErrClosed", err)
	}
	if err := file.Close(); err != nil {
		t.Errorf("second Close must be idempotent: %v", err)
	}
}
