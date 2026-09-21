# Architecture, Ownership and Concurrency Boundaries

## Component map

```
Caller
  │  OpenTable(Config{Filename|Data|Reader|IO})
  ▼
io.go  OpenTable ── selects ──► UnixIO (io_unix.go)          [*os.File]
                              WindowsIO (io_windows.go)      [windows HANDLE]
                              GenericIO (io_generic.go)      [io.ReadWriteSeeker]
  │
  ▼
File (file.go)  ── owns ──► Header / MemoHeader (header.go)
  │                        Table{columns, mods, rowPointer} (table.go)
  │                        handle / relatedHandle
  │                        mu sync.RWMutex, memoMutex sync.Mutex, closed
  │
  ├─ Row/Next/Rows/Search/GoTo/Skip/Deleted/ReadRow ── locked entry points
  ├─ WriteRow/WriteMemo/Close/WriteColumns-as-needed ─ locked entry points
  └─ Interpret/Represent (interpreter.go)
        ├─ Character/Numeric/Date/Logical: pure conversion
        ├─ Memo:   ReadMemo / WriteMemo  ──► IO ──► FPT blocks
        └─ Varchar/Varbinary: ReadNullFlag ──► IO ──► trailing _NullFlags byte

EncodingConverter (encoding.go): stateless code page conversion,
                                 consulted by Character/Memo/Varchar paths.
BytesReadWriteSeeker (bytes_reader.go): owns an in-memory copy of Data;
                                        single shared cursor (see below).
```

## Ownership

- `File` owns the open handles and the cached header/table metadata. `Row` and
  `Field` values are snapshots handed out to callers; mutating them never
  touches disk until `Row.Write`/`Add`.
- `GenericIO` does not own the `io.ReadWriteSeeker` handles; it uses whatever
  the caller supplies and `Close` only closes handles that implement
  `io.Closer`. `OpenTable` with `Data` copies the bytes into a
  `BytesReadWriteSeeker`; writes do not mutate the original slice — read back
  through the handle (`Data()`) or persist via a custom handle.
- `UnixIO`/`WindowsIO` open and own their OS handles for the lifetime of the
  `File`; `Close` releases both DBF and FPT handles.
- The IO implementations are value types (`UnixIO{}`, `WindowsIO{}`,
  `GenericIO{...}`) and hold no per-file state themselves. All mutable state
  lives on `File` (including the mutexes). Custom IO implementations therefore
  do not need their own locks for state reached through one `File`, but must be
  safe for their own external sharing.
- `EncodingConverter` is shared/read-only after construction.

## Lock ranges

`File.mu` is a `sync.RWMutex`; `File.memoMutex` is a separate `sync.Mutex`
guarding FPT allocation (`NextFree` advancement) between writers.

| Entry point | Lock | Protected state |
| --- | --- | --- |
| `Row`, `ReadRow`, `ReadMemo`, `ReadNullFlag`, `Deleted`, `Search`, `EOF`, `BOF`, `Pointer`, `Header`, `RowsCount` | `RLock` | Stable reads of `header`, `table`, handles; `Search` moves the cursor via `goToLocked` but changes no bytes |
| `Next`, `Rows`, `WriteRow`, `WriteMemo`, `Close`, `Skip`, `GoTo` | `Lock` | `header.RowsCount`, `rowPointer`, handle lifecycle (`Next`/`Rows` advance the shared cursor) |
| IO `WriteMemo` internals | additionally `memoMutex` | `memoHeader.NextFree` read-modify-write + FPT entry append |
| `WindowsIO` with `Config.WriteLock=true` | OS `LockFileEx` byte range | Cross-process record/header range locks (Windows only) |

Lock order is always `File.mu` → `File.memoMutex` (never reversed). Internal
helpers (`readRowLocked`, `rowLocked`, `readMemoLocked`, `writeMemoLocked`,
`readNullFlagLocked`, `goToLocked`, `skipLocked`, `interpretLocked`,
`representLocked`) assume the caller already holds `File.mu` (R or W); public
methods take the lock themselves.

### Explicitly not covered

The lock boundaries are intentionally narrow. The following are **raw IO
primitives** with no locking and no `closed` check, because they are used while
constructing a table or while already holding a lock:

- `Create`, `ReadHeader`, `WriteHeader`, `ReadColumns`, `WriteColumns`,
  `ReadMemoHeader`, `WriteMemoHeader`, `Init`.
- `Row.Increment` calls `WriteColumns` directly; callers should perform schema
  autoincrement steps from a single goroutine or hold external synchronization.

Direct access to `File.Header()` returns the live pointer; callers must not
mutate it. `Columns()` likewise returns the live column slice. Treat both as
read-only views.

## Exact concurrency guarantees

1. One `*File` may be used by multiple goroutines when every operation goes
   through the locked public entry points listed above. Concurrent
   `Row`/`Next`/`Search`/`Deleted` readers are allowed; writers are serialized
   against readers and each other through `File.mu`.
2. The internal row pointer is shared table state, not per-goroutine state.
   Concurrent goroutines that mix `Next`/`Skip`/`GoTo` are serialized but still
   observe each other's cursor movement. For independent scans, open separate
   `File` instances (separate handles over the same bytes) or use absolute
   `ReadRow(position)` / `Search`, which do not rely on the shared pointer
   beyond the call.
3. **The `Table`/`File` object is not "fully thread safe" by blanket guarantee.**
   Anything that bypasses the listed entry points is unsynchronized:
   - raw primitives (`WriteHeader`, `WriteColumns`, `Init`, ...),
   - mutating returned `Header`/`Column` pointers,
   - a custom `IO` implementation with its own shared state,
   - sharing one `BytesReadWriteSeeker` cursor across goroutines outside of a
     `File`'s locked calls.
4. `Close` is serialized with reads and writes: it takes `Lock`, so it waits
   for in-flight operations to finish; after `closed` is set, every locked
   entry point returns an error wrapping `ErrClosed` without touching the
   handles. `Close` is idempotent. Closing a file does not, on its own, flush
   `Row` values that were never written.
5. `BytesReadWriteSeeker` is safe for sequential use and supports sparse writes
   (seek past end then write, zero filled like an OS file). It has one shared
   internal cursor; it is not a multi-reader handle. Parallel readers that need
   independent cursors should open separate `File` values or supply a custom
   concurrency safe handle (see `fanoutStore` in
   `dbase/lifecycle_test.go`).
6. Cross-process locking exists only on Windows when `WriteLock: true`
   (`LockFileEx` over the header and the target record range). On Unix there is
   no `flock`/`fcntl`; multiple processes opening the same file are not
   coordinated by this library.

## Determinism for tests

The concurrency tests in `dbase/lifecycle_test.go` use channel barriers and a
concurrency safe in-memory store with per-goroutine cursors, never sleeps or
wall-clock waits:

- `TestConcurrencyBarrierTwoReadersOneWriter` parks two readers inside the
  record read, asserts the writer cannot reach the store, releases the readers,
  and checks both observed the stable pre-write value before the single append
  commits (`RowsCount` goes 1 → 2 exactly once).
- `TestCloseInterleavedWithRead` parks one reader, asserts `Close` cannot return
  while the read is in flight, releases it, then asserts the read completed
  successfully and every later entry point fails with `ErrClosed`; a second
  `Close` is a no-op.

These tests record the guarantees the repository currently commits to; they
fail immediately if the locking scope is reduced.
