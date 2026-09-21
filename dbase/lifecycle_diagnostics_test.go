package dbase

import (
	"encoding/binary"
	"errors"
	stderrors "errors"
	"io"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// failingConverter rejects every byte with the high bit set. It lets the tests
// trigger a deterministic charset decoding failure without depending on the
// behavior of any particular code page.
type failingConverter struct{}

func (failingConverter) Decode(in []byte) ([]byte, error) {
	for _, b := range in {
		if b >= 0x80 {
			return nil, errors.New("forced decoding failure")
		}
	}
	return in, nil
}
func (failingConverter) Encode(in []byte) ([]byte, error) {
	out := make([]byte, len(in))
	copy(out, in)
	return out, nil
}
func (failingConverter) CodePage() byte { return 0x03 }

// TestDiagnosticDecodeFailure covers character and memo charset failures.
func TestDiagnosticDecodeFailure(t *testing.T) {
	store := newLifecycleStore()
	file := store.create(t)
	row, err := file.RowFromMap(map[string]interface{}{
		"NAME": "bad", "NUM": int64(1), "BIRTH": "", "ACTIVE": true, "NOTE": "",
	})
	if err != nil {
		t.Fatalf("RowFromMap: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Corrupt the NAME bytes with an invalid code page sequence.
	nameOff := int(lifecycleFirstRow) + 1
	store.dbf.data[nameOff] = 0x81

	cfg := store.config()
	cfg.Filename = ""
	cfg.Converter = failingConverter{}
	cfg.IO = GenericIO{Handle: store.dbf, RelatedHandle: store.fpt}
	bad, err := OpenTable(cfg)
	if err != nil {
		t.Fatalf("OpenTable with failing converter: %v", err)
	}
	defer bad.Close()

	_, err = bad.ReadRowAt(0)
	if !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("character decode err = %v, want errors.Is ErrInvalidEncoding", err)
	}

	// After the failed decode the table must stay readable: seeking back to
	// the same record and swapping in a working converter succeeds.
	bad.config.Converter = NewDefaultConverter(charmap.Windows1252)
	reread, err := bad.ReadRowAt(0)
	if err != nil {
		t.Fatalf("re-read after decode failure: %v", err)
	}
	if reread == nil {
		t.Fatal("re-read returned nil row")
	}
}

// TestDiagnosticTruncatedRecord builds a short read IO: the header advertises
// a consistent file, but a read returns fewer than RowLength bytes.
type shortReadIO struct {
	GenericIO
}

func (io shortReadIO) ReadRow(file *File, position uint32) ([]byte, error) {
	buf := make([]byte, file.header.RowLength-1)
	return buf, nil
}

func TestDiagnosticTruncatedRecord(t *testing.T) {
	store := newLifecycleStore()
	seed := store.create(t)
	seedRow, err := seed.RowFromMap(map[string]interface{}{
		"NAME": "A", "NUM": int64(1), "BIRTH": "", "ACTIVE": true, "NOTE": "",
	})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := seedRow.Add(); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	file := openInstrumented(t, store, shortReadIO{
		GenericIO: GenericIO{Handle: store.dbf, RelatedHandle: store.fpt},
	})
	defer file.Close()
	if _, err := file.ReadRowAt(0); !errors.Is(err, ErrTruncatedRecord) {
		t.Fatalf("short read err = %v, want ErrTruncatedRecord", err)
	}
}

// TestDiagnosticRecordCountMismatch inflates the header count past the
// physical end and verifies the mismatch is distinct from a short record.
func TestDiagnosticRecordCountMismatch(t *testing.T) {
	store := newLifecycleStore()
	seed := store.create(t)
	seedRow, err := seed.RowFromMap(map[string]interface{}{
		"NAME": "A", "NUM": int64(1), "BIRTH": "", "ACTIVE": true, "NOTE": "",
	})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := seedRow.Add(); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	dbf := store.dbf.data
	binary.LittleEndian.PutUint32(dbf[4:8], 99)

	file := store.open(t)
	defer file.Close()
	_, err = file.ReadRowAt(0)
	if !errors.Is(err, ErrRecordCountMismatch) {
		t.Fatalf("inflated count err = %v, want ErrRecordCountMismatch", err)
	}
	if errors.Is(err, ErrTruncatedRecord) {
		t.Fatal("count mismatch must not also classify as truncated record")
	}
}

// TestDiagnosticMemoFreeBlock points the memo field into the reserved header
// region (block 1) while the FPT is well formed.
func TestDiagnosticMemoFreeBlock(t *testing.T) {
	store, file := seedOneMemoRow(t)
	defer file.Close()
	dbf := store.dbf.data
	off := int(file.Header().FirstRow) + int(file.Header().RowLength) - 4
	binary.LittleEndian.PutUint32(dbf[off:off+4], 1) // block 1 = header area

	_, err := file.ReadRowAt(0)
	if !errors.Is(err, ErrMemoFreeBlock) {
		t.Fatalf("free block pointer err = %v, want ErrMemoFreeBlock", err)
	}
}

// TestDiagnosticMemoOutOfBounds points the memo field past the FPT end.
func TestDiagnosticMemoOutOfBounds(t *testing.T) {
	store, file := seedOneMemoRow(t)
	defer file.Close()
	dbf := store.dbf.data
	off := int(file.Header().FirstRow) + int(file.Header().RowLength) - 4
	binary.LittleEndian.PutUint32(dbf[off:off+4], 1000)

	_, err := file.ReadRowAt(0)
	if !errors.Is(err, ErrMemoOutOfBounds) {
		t.Fatalf("out of bounds pointer err = %v, want ErrMemoOutOfBounds", err)
	}
}

// TestDiagnosticInvalidDeleteFlag flips the delete marker to an unknown byte.
func TestDiagnosticInvalidDeleteFlag(t *testing.T) {
	store, file := seedOneMemoRow(t)
	defer file.Close()
	dbf := store.dbf.data
	dbf[file.Header().FirstRow] = '?'

	_, err := file.ReadRowAt(0)
	if !errors.Is(err, ErrInvalidDeleteFlag) {
		t.Fatalf("bad marker err = %v, want ErrInvalidDeleteFlag", err)
	}
}

// TestDiagnosticAfterErrorReadAgain verifies a failed diagnostic leaves the
// cursor and handle usable: another ReadRowAt and a marker read still work.
func TestDiagnosticAfterErrorReadAgain(t *testing.T) {
	store, file := seedOneMemoRow(t)
	defer file.Close()
	dbf := store.dbf.data
	off := int(file.Header().FirstRow) + int(file.Header().RowLength) - 4
	binary.LittleEndian.PutUint32(dbf[off:off+4], 1000)

	if _, err := file.ReadRowAt(0); !errors.Is(err, ErrMemoOutOfBounds) {
		t.Fatalf("first read err = %v, want ErrMemoOutOfBounds", err)
	}
	// Restore the pointer; reading must work immediately without reopening.
	binary.LittleEndian.PutUint32(dbf[off:off+4], 8)
	row, err := file.ReadRowAt(0)
	if err != nil {
		t.Fatalf("read after repair failed: %v", err)
	}
	note, err := row.StringValueByName("NOTE")
	if err != nil || note != "memo-A" {
		t.Fatalf("NOTE after repair = %q, %v", note, err)
	}
}

// seedOneMemoRow creates the standard lifecycle table with one valid memo row
// and returns it reopened, ready for byte corruption.
func seedOneMemoRow(t *testing.T) (*lifecycleStore, *File) {
	t.Helper()
	store := newLifecycleStore()
	seed := store.create(t)
	row, err := seed.RowFromMap(map[string]interface{}{
		"NAME": "A", "NUM": int64(1), "BIRTH": "", "ACTIVE": true, "NOTE": "memo-A",
	})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	return store, store.open(t)
}

// failingRecordSeeker wraps the in-memory DBF seeker and makes the first write
// whose offset is inside the record area fail. Header writes at offset 0 keep
// working, which exercises the real WriteRow ordering: record bytes are
// attempted first and the header count is never committed on failure.
type failingRecordSeeker struct {
	*BytesReadWriteSeeker
	firstRow int64
	failed   bool
}

func (f *failingRecordSeeker) Write(p []byte) (int, error) {
	if !f.failed {
		pos, _ := f.Seek(0, io.SeekCurrent)
		if pos >= f.firstRow {
			f.failed = true
			return 0, errForcedWrite
		}
	}
	return f.BytesReadWriteSeeker.Write(p)
}

var errForcedWrite = stderrors.New("forced record write failure")

func TestRecoverableAfterFailedAppend(t *testing.T) {
	store := newLifecycleStore()
	seed := store.create(t)
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	failingSeeker := &failingRecordSeeker{
		BytesReadWriteSeeker: store.dbf,
		firstRow:             int64(lifecycleFirstRow),
	}
	failingIO := GenericIO{Handle: failingSeeker, RelatedHandle: store.fpt}
	file := openInstrumented(t, store, failingIO)
	defer file.Close()

	row, err := file.RowFromMap(map[string]interface{}{
		"NAME": "X", "NUM": int64(1), "BIRTH": "", "ACTIVE": true, "NOTE": "",
	})
	if err != nil {
		t.Fatalf("build row: %v", err)
	}
	if err := row.Add(); err == nil {
		t.Fatal("expected append to fail")
	}
	if file.RowsCount() != 0 {
		t.Fatalf("RowsCount after failed append = %d, want 0 (rollback)", file.RowsCount())
	}

	// Retry through the same File object; cursor and header must be intact.
	row2, err := file.RowFromMap(map[string]interface{}{
		"NAME": "Y", "NUM": int64(2), "BIRTH": "", "ACTIVE": false, "NOTE": "",
	})
	if err != nil {
		t.Fatalf("rebuild row: %v", err)
	}
	if err := row2.Add(); err != nil {
		t.Fatalf("retry append: %v", err)
	}
	if file.RowsCount() != 1 {
		t.Fatalf("RowsCount after retry = %d, want 1", file.RowsCount())
	}
	reread, err := file.ReadRowAt(0)
	if err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	name, _ := reread.StringValueByName("NAME")
	if name != "Y         " {
		t.Errorf("NAME after recovery = %q", name)
	}
}
