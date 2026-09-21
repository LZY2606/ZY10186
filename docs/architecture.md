# Architecture, ownership and locking

This document states precisely who owns which resource and where the
synchronization boundaries are. It deliberately does **not** claim "the whole
`Table`/`File` is safe for concurrent use": only the entry points listed below
take the lifecycle lock.

## Components

| component | file | responsibility | owns |
| --- | --- | --- | --- |
| `File` | `file.go` | aggregate: config, handles, header, memo header, table, null flag column, lifecycle state | both handles, in-memory state |
| `Table` | `table.go` | column list, per-column `Modification`, shared `rowPointer`; `Row` serialization | cursor, column metadata |
| `Header` / `MemoHeader` | `header.go` | decoded 30/8 byte headers, geometry helpers | none (value snapshots) |
| `Interpret` / `Represent` | `interpreter.go` | raw bytes <-> Go values; charset and memo I/O trigger | no state; calls converter and IO |
| `EncodingConverter` | `encoding.go` | decode/encode against the code page mark | converter internals only; must be safe for concurrent use |
| `BytesReadWriteSeeker` | `bytes_reader.go` | in-memory `io.ReadWriteSeeker` backend for `GenericIO` | its byte slice, offset and internal `bytes.Reader` |
| `GenericIO` | `io_generic.go` | IO over any `io.ReadWriteSeeker` (bytes, custom readers) | no OS resources |
| `UnixIO` | `io_unix.go` | `*os.File` backend, build tag `!windows` | opened DBF/FPT file descriptors |
| `WindowsIO` | `io_windows.go` | `windows.Handle` backend, `LockFileEx` record locks when `WriteLock` is set | opened DBF/FPT handles |

`database.go` (`Database`) opens several `File` values; each `File` owns its
own handles and its own lifecycle lock. There is no shared lock across tables.

## Lock inventory

1. **Lifecycle RWMutex** (`file.opMu`, added in `lifecycle.go`).
   * Read entry points take `RLock`; write entry points, cursor movement and
     `Close` take `Lock`.
   * It protects `handle`, `relatedHandle`, `header`, `memoHeader`,
     `table.rowPointer`, `table.columns`, `table.mods` and the `closed` flag.
2. **DBF writer mutex** (`file.dbaseMutex`) and **FPT writer mutex**
   (`file.memoMutex`), held inside the backend `WriteRow`/`WriteMemo` methods.
   With the lifecycle lock these are now belt-and-braces; they remain to
   preserve the documented backend contract and are harmless because the
   lifecycle lock is already exclusive on that path.
3. **OS record locks**: Windows only, and only when `Config.WriteLock` is
   true. `UnixIO` performs no `flock`/`fcntl` locking; inter-process
   coordination is therefore not provided on Unix.
4. **Bytes backend mutex**: `BytesReadWriteSeeker` serializes its own
   Seek/Read/Write so concurrent lifecycle readers do not race on its offset.

## Exactly which entry points are synchronized

Write-locked (exclusive; mutate header/FPT/cursor or move state):

`Close`, `WriteHeader`, `WriteColumns`, `WriteMemoHeader`, `WriteRow`
(also `Row.Write`, `Row.Add`), `WriteMemo`, `GoTo`, `Skip`, `Deleted`,
`Search`, `Rows`, `Next`, `Row`, `Represent`, `Row.ToBytes`, `Row.Increment`,
`RowFromMap`, `RowFromJSON`, `RowFromStruct`, `SetColumnModification`,
`SetColumnModificationByName`.

Read-locked (shared; can run in parallel with each other):

`ReadRowAt`, `ReadRow`, `ReadHeader`, `ReadColumns`, `ReadMemoHeader`,
`ReadMemo`, `ReadNullFlag`, `Interpret`, `BytesToRow`, all read-only accessors
(`Header`, `RowsCount`, `Columns`, `Column`, `ColumnPosByName`, `Pointer`,
`EOF`, `BOF`, `TableName`, ...), `NewRow`, `NewField*`, and `Row` value
accessors (`ValueByName`, `FieldByName`, `ToMap`, `ToJSON`, `ToStruct`).

Internal construction paths (`NewTable`, `Init`, `Create`) run before the
`File` is published and intentionally do not lock.

## What this means in practice

* **Safe**: multiple goroutines calling `ReadRowAt` on one `File`; any single
  writer while readers exist; `Close` racing any entry point (the operation in
  flight completes, later operations return `ErrClosed`).
* **Not a free for all**: `Row`, `Next`, `Rows`, `GoTo`, `Skip` and `Deleted`
  share one cursor. They are serialized, but two goroutines moving the cursor
  observe each other's positions by design. Use `ReadRowAt` for parallel scans.
* A returned `Row` is a detached snapshot plus its memo pointers; mutating its
  `Field` values is caller owned until `Write`. Do not share one mutable `Row`
  across goroutines without external synchronization.
* The OS-level file offset after any call is unspecified; correctness relies
  on explicit seeks, not on a remembered offset.
* The `EncodingConverter` implementation must be concurrency safe; the built-in
  converters are stateless.

## Deterministic close/read interleaving

`Close` and an in-flight read have exactly one possible order, enforced by the
RWMutex (no sleeps or retries):

* Read acquires `RLock` and starts -> `Close` blocks on `Lock` -> read returns
  successfully -> `Close` closes handles and sets `closed=true`.
* `Close` first -> every subsequent entry point returns `ErrClosed`.

`TestCloseVsInFlightReadDeterministic` pins this with a blocking IO hook that
releases the read only after `Close` has been observed waiting.

## Diagnostic sentinels

Failures that used to share generic messages are now distinct and reachable via
`errors.Is` (`Error.Unwrap` returns all attached details):

| sentinel | condition |
| --- | --- |
| `ErrInvalidEncoding` | character or text memo charset decoding fails |
| `ErrTruncatedRecord` | a record read yields fewer than `RowLength` bytes |
| `ErrRecordCountMismatch` | header-declared table size exceeds physical DBF size |
| `ErrMemoFreeBlock` | memo pointer is below the first data block (header/free space) |
| `ErrMemoOutOfBounds` | memo block header starts beyond the physical FPT end |
| `ErrInvalidDeleteFlag` | record byte 0 is neither `0x20` nor `0x2A` |
| `ErrClosed` | any entry point is used after `Close` |

The delete marker (`0x2A`, a logical flag) and physical removal are separate
concepts throughout: the library never physically removes or moves records, so
there is no purge failure class.

## Platform I/O differences

* File naming: `UnixIO`/`WindowsIO` create upper-case paths and require `.DBF`;
  `GenericIO` never touches the file system.
* File discovery on Unix uses a case-insensitive directory scan
  (`findFile`); tests avoid the file system entirely, so no directory ordering
  assumption exists.
* Windows may apply `LockFileEx` range locks with `WriteLock`; Unix relies
  solely on the in-process lifecycle lock. Multi-process writers on Unix are
  not coordinated by this package.
