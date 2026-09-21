# Diagnostic Error Reference

All package errors are the concrete `dbase.Error` type (`dbase/error.go`) with a
human readable message and a list of wrapped causes. Use `errors.Is` to branch
on the structural cause; `Unwrap() []error` exposes every detail.

| Sentinel | When it is returned | Typical trigger |
| --- | --- | --- |
| `ErrEOF` | Pointer at/beyond `RowsCount` (`ReadRow`, `GoTo`, `Deleted`, `Next`) | Iterating past the last record |
| `ErrBOF` | Declared for before-first navigation | Reserved for callers wrapping cursor logic |
| `ErrIncomplete` | Windows record-marker short read | Damaged DBF record |
| `ErrClosed` | Any locked entry point after `Close` | Use after close |
| `ErrRowTruncated` | A record read returns fewer than `RowLength` bytes | Record slot truncated/damaged while file size still satisfies the header |
| `ErrTableTruncated` | At open: physical size `< FirstRow + RowsCount·RowLength` | Header declares more records than bytes exist |
| `ErrInvalidMarker` | Record first byte is neither `0x20` (active) nor `0x2A` (deleted) | Corrupted record marker |
| `ErrInvalidEncoding` | Converter `Decode` rejects field or memo bytes | Character/memo bytes not representable in the configured code page |
| `ErrMemoOutOfBounds` | Memo pointer `< firstDataBlock` (inside the 512 byte FPT header) or entry header/data beyond the physical FPT end | Dangling pointer, truncated FPT |
| `ErrMemoFree` | Memo pointer `>= NextFree` but still within the file | Pointer into unwritten/free space (never allocated or header reset) |
| `ErrNoDBF` / `ErrNoFPT` | Handle missing or nil of the expected type | Memo flag set but no FPT supplied/opened |
| `ErrInvalidPosition` | Column position helpers out of range | Bad column index/name |
| `ErrUnknownDataType` | Unknown field type byte | Unsupported column type |

## Deletion vs physical removal — different signals

- A deleted row is a record whose first byte is `0x2A`. `Deleted()` reads only
  that byte; `Rows(false, true)` filters such records; the record stays on disk
  and `Header.RowsCount` still counts it.
- There is no PACK operation in this version: nothing rewrites subsequent
  records left, nothing shrinks the DBF, and no FPT blocks are reclaimed.
  "Removed" data is therefore always a marker (`0x2A`), never erased bytes.
  Tests assert this distinction directly (see
  `TestRecordLifecycleUpdateAndDelete`).

## Recoverability rules

- Reads are non-destructive and use an absolute `Seek` before each read; after
  any read error, retrying from the same pointer (or another pointer) works and
  the OS file offset cannot remain "poisoned".
- A failed append does not advance `Header.RowsCount` in memory; the header is
  rewritten only after the record bytes landed, so reopen and memory cannot
  disagree on the count.
- A failed header rewrite after a successful record write leaves unreferenced
  tail bytes; they are invisible to every reader (count is authoritative) and
  can be overwritten by the next append at the same offset.
- Charset errors do not consume the record slot differently: the pointer stays
  in place, so callers can re-open with a different converter and retry.

## Test coverage map

| Situation | Test |
| --- | --- |
| Charset decode failure | `TestDiagnosticCharsetFailure` |
| Truncated record bytes | `TestDiagnosticTruncatedRecord` |
| Header count larger than file | `TestDiagnosticHeaderDeclaresMoreRowsThanFile` |
| Invalid delete marker | `TestDiagnosticInvalidDeleteMarker` |
| Memo pointer into free space | `TestDiagnosticMemoPointerFreeAndOutOfBounds` (`ErrMemoFree`) |
| Memo pointer inside header / beyond FPT end | same test (`ErrMemoOutOfBounds`) |
| Logical delete marker vs physical bytes | `TestRecordLifecycleUpdateAndDelete` |
| Close/use-after-close | `TestCloseInterleavedWithRead` |
