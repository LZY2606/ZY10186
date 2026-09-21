package dbase

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// barrier is a rendezvous primitive: every participant calls Wait, which blocks
// until exactly need participants have arrived, then releases all of them at
// once. It makes interleavings deterministic without sleeps or wall clock
// assumptions.
type barrier struct {
	mu      sync.Mutex
	cond    *sync.Cond
	need    int
	arrived int
	phase   int
}

func newBarrier(need int) *barrier {
	b := &barrier{need: need}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Wait blocks until every expected participant arrives for the current phase.
func (b *barrier) Wait() {
	b.mu.Lock()
	phase := b.phase
	b.arrived++
	if b.arrived == b.need {
		b.arrived = 0
		b.phase++
		b.cond.Broadcast()
		b.mu.Unlock()
		return
	}
	for phase == b.phase {
		b.cond.Wait()
	}
	b.mu.Unlock()
}

// itoa avoids pulling strconv into the lifecycle tests only for row labels.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// blockReadIO stalls every physical record read at the chosen positions until
// release is closed. Readers block strictly inside the read entry point while
// holding the lifecycle read lock, which is exactly what the close
// determinism and overlap tests need to observe.
type blockReadIO struct {
	GenericIO
	blocked map[uint32]bool
	entered chan<- uint32
	release <-chan struct{}
	once    sync.Map
}

func (io *blockReadIO) ReadRow(file *File, position uint32) ([]byte, error) {
	if io.blocked[position] {
		if _, loaded := io.once.LoadOrStore(position, struct{}{}); !loaded {
			io.entered <- position
			<-io.release
		}
	}
	return io.GenericIO.ReadRow(file, position)
}

// populatedStore builds a lifecycle table with rowsCount rows, each carrying a
// memo. All bytes are committed before any concurrency phase starts.
func populatedStore(t *testing.T, rowsCount int) *lifecycleStore {
	t.Helper()
	store := newLifecycleStore()
	file := store.create(t)
	for i := 0; i < rowsCount; i++ {
		row, err := file.RowFromMap(map[string]interface{}{
			"NAME": "R" + itoa(i),
			"NUM":  int64(i),
			"BIRTH": time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC).
				AddDate(0, 0, i).Format(time.RFC3339),
			"ACTIVE": i%2 == 0,
			"NOTE":   "memo-" + itoa(i),
		})
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if err := row.Add(); err != nil {
			t.Fatalf("add row %d: %v", i, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	return store
}

func openInstrumented(t *testing.T, store *lifecycleStore, ioImpl IO) *File {
	t.Helper()
	// Open with plain GenericIO (OpenTable resets file.io to the embedded
	// GenericIO of a custom IO value), then install the instrumented backend
	// so the read hooks actually run.
	cfg := store.config()
	cfg.Filename = ""
	cfg.IO = GenericIO{Handle: store.dbf, RelatedHandle: store.fpt}
	file, err := OpenTable(cfg)
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}
	file.io = ioImpl
	return file
}

// TestBarrierReadersWriterInterleave pins down the concurrency promise using
// explicit barriers instead of timing:
//
//   - phase A: both ReadRowAt calls enter the physical read concurrently (read
//     locks are shared) and return their own record without moving a cursor;
//   - phase B: the writer appends only after both readers finished;
//   - phase C: every reader observes the committed row count through a fresh
//     read.
func TestBarrierReadersWriterInterleave(t *testing.T) {
	const total = 4
	store := populatedStore(t, total)

	entered := make(chan uint32, 2)
	release := make(chan struct{})
	blocked := &blockReadIO{
		GenericIO: GenericIO{Handle: store.dbf, RelatedHandle: store.fpt},
		blocked:   map[uint32]bool{0: true, 1: true},
		entered:   entered,
		release:   release,
	}
	file := openInstrumented(t, store, blocked)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = file.Close()
	}()

	bar := newBarrier(3)
	errs := make(chan error, 4)
	names := make(chan string, 2)
	reader := func(pos uint32) {
		row, err := file.ReadRowAt(pos)
		if err != nil {
			errs <- err
			return
		}
		name, err := row.StringValueByName("NAME")
		if err != nil {
			errs <- err
			return
		}
		names <- name
		bar.Wait() // phase B: writer runs
		bar.Wait() // phase C: committed state visible
		if file.RowsCount() != total+1 {
			errs <- errors.New("reader did not observe committed row count")
			return
		}
		errs <- nil
	}
	go reader(0)
	go reader(1)

	// Both physical reads must have entered before the writer starts.
	seen := map[uint32]int{}
	for i := 0; i < 2; i++ {
		select {
		case pos := <-entered:
			seen[pos]++
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both readers to enter the physical read")
		}
	}
	if seen[0] != 1 || seen[1] != 1 {
		t.Fatalf("expected readers at rows 0 and 1, got %v", seen)
	}
	close(release)

	// Phase B: append after both reads are guaranteed to be inside.
	bar.Wait()
	appendRow, err := file.RowFromMap(map[string]interface{}{
		"NAME": "R4", "NUM": int64(4),
		"BIRTH":  time.Date(2000, time.January, 5, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"ACTIVE": true, "NOTE": "memo-4",
	})
	if err != nil {
		t.Fatalf("append build: %v", err)
	}
	if err := appendRow.Add(); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if file.RowsCount() != total+1 {
		t.Fatalf("RowsCount after append = %d, want %d", file.RowsCount(), total+1)
	}
	bar.Wait()

	gotNames := map[string]bool{}
	for i := 0; i < 2; i++ {
		gotNames[<-names] = true
	}
	if !gotNames["R0        "] || !gotNames["R1        "] {
		t.Errorf("readers returned %v, want padded R0 and R1", gotNames)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
	}

	appended, err := file.ReadRowAt(total)
	if err != nil {
		t.Fatalf("read appended row: %v", err)
	}
	name, _ := appended.StringValueByName("NAME")
	if name != "R4        " {
		t.Errorf("appended NAME = %q, want padded R4", name)
	}
}

// TestCloseVsInFlightReadDeterministic proves the close/read interleaving has
// one fixed outcome: Close waits for the in-flight read, the read succeeds,
// and every operation started after Close fails with ErrClosed. No sleep is
// used; the blocking IO hook is the synchronization point.
func TestCloseVsInFlightReadDeterministic(t *testing.T) {
	store := populatedStore(t, 2)
	entered := make(chan uint32, 1)
	release := make(chan struct{})
	blocked := &blockReadIO{
		GenericIO: GenericIO{Handle: store.dbf, RelatedHandle: store.fpt},
		blocked:   map[uint32]bool{0: true},
		entered:   entered,
		release:   release,
	}
	file := openInstrumented(t, store, blocked)

	type result struct {
		row *Row
		err error
	}
	readDone := make(chan result, 1)
	go func() {
		row, err := file.ReadRowAt(0)
		readDone <- result{row, err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader never entered the physical read")
	}

	closeFinished := make(chan error, 1)
	go func() { closeFinished <- file.Close() }()

	// Close cannot complete while the read holds the read lock. A non-blocking
	// probe is sufficient: the read is provably still blocked on release.
	select {
	case err := <-closeFinished:
		t.Fatalf("Close finished before the in-flight read: %v", err)
	default:
	}

	close(release)
	res := <-readDone
	if res.err != nil {
		t.Fatalf("in-flight read failed: %v", res.err)
	}
	if res.row == nil || res.row.Deleted {
		t.Fatalf("in-flight read returned unusable row: %+v", res.row)
	}
	if err := <-closeFinished; err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := file.ReadRowAt(0); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close err = %v, want ErrClosed", err)
	}
	if err := file.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second close err = %v, want ErrClosed", err)
	}
	if err := file.GoTo(0); !errors.Is(err, ErrClosed) {
		t.Fatalf("GoTo after close err = %v, want ErrClosed", err)
	}
}
