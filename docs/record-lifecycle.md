# Record Lifecycle Map

This document tracks one record through `OpenTable` → seek (`GoTo`/`Skip`) →
`Row`/`Next` → write (`Write`/`Add`) → logical delete → close, and states what
changes in **memory**, the **DBF bytes**, and the **FPT memo blocks** at every
step. All offsets follow the FoxPro (`0x30/0x31/0x32`) layout: little-endian
integers, 30 byte file header, 32 bytes per field descriptor, `0x0D` header
terminator, records at `FirstRow` (header field at offset 8), `RowLength` bytes
per record (offset 10), and a one byte delete marker per record.

## Structures and who owns what

| Layer | Type(s) | Owns | Persistent effect |
| --- | --- | --- | --- |
| Public table | `Table` (`dbase/table.go`) | `columns []*Column`, `mods`, `rowPointer` | None by itself |
| File facade | `File` (`dbase/file.go`, lock map in `dbase/io.go`) | `header *Header`, `memoHeader *MemoHeader`, `handle`, `relatedHandle`, `mu sync.RWMutex`, `memoMutex sync.Mutex`, `closed` | Delegates every byte operation to an `IO` |
| Header | `Header`/`MemoHeader` (`dbase/header.go`) | Decoded metadata cached in memory | Written to DBF offset 0 / FPT offset 0 |
| Interpreter | `File.Interpret`/`Represent` (`dbase/interpreter.go`) | No state; converts `[]byte` ↔ Go values, reads/writes memo entries on demand | FPT block reads/appends through the IO |
| Encoding | `EncodingConverter` (`dbase/encoding.go`) | Code page mark ↔ `x/text` encoder | None (stateless) |
| Bytes reader | `BytesReadWriteSeeker` (`dbase/bytes_reader.go`) | Owns a **copy** of the input byte slice and one shared cursor | In memory only; retrieve via `Data()` |
| Generic IO | `GenericIO` (`dbase/io_generic.go`) | Uses caller supplied `io.ReadWriteSeeker` handles | Whatever the handles persist |
| Unix IO | `UnixIO` (`dbase/io_unix.go`) | Opens `*os.File` handles | Real `.DBF`/`.FPT` files |
| Windows IO | `WindowsIO` (`dbase/io_windows.go`) | Opens Windows `HANDLE`s, optional `LockFileEx` byte-range locks | Real `.DBF`/`.FPT` files |

`Row` is a detached value object: it holds decoded `fields`, a `Deleted` flag,
and `Position`. A `Row` does **not** own the cursor; writing it does not move
`rowPointer`.

## Byte map of one table

```
DBF
0            1  2  3  4..7        8..9       10..11     28         29
+------------+--+--+--+-----------+-----------+-----------+----------+------------+
| FileType   |YY|MM|DD| RowsCount | FirstRow  | RowLength |TableFlags| CodePage   |
+------------+--+--+--+-----------+-----------+-----------+----------+------------+
32..FirstRow-1: field descriptors (32B each) + hidden _NullFlags (0x30 type) + 0x0D
FirstRow ..   : records, each [marker 0x20|0x2A][column bytes][_NullFlags bytes]

FPT (only when TableFlags & 0x02)
0..3 NextFree (BE)  6..7 BlockSize (BE); the first 512 bytes are always the
header region. First data block = ceil(512/BlockSize): block 8 for BlockSize 64.
Each entry: [signature BE u32: 1=text 0=binary][length BE u32][payload][padding]
```

## Lifecycle table

| Operation | Memory | DBF bytes | FPT blocks |
| --- | --- | --- | --- |
| `NewTable` | `Header` built (date today, `RowsCount=0`, `FirstRow=32+(fields+1)·32` when a null flag exists), `Table.columns`, `memoHeader.NextFree=firstDataBlock`, `BlockSize` normalized 0→64 | File created; 30 byte header, field descriptors, `0x0D`, zero padding to `FirstRow` written by `Init` | FPT created: 8 byte header (`NextFree=firstDataBlock`, block size) plus zero fill to 512 bytes |
| `NewRow`/`RowFromMap` | New detached `Row` at `Position=RowsCount+1`, fields decoded/defaulted; autoincrement calls `WriteColumns` when applicable | Nothing | Nothing |
| `row.Add()` | Sets `Position=RowsCount+1` then `WriteRow` | Record bytes at `FirstRow + (Position-1)·RowLength`; only after the write succeeds `RowsCount++` and the 30 byte header is rewritten | A non-empty memo appends `memoBlocksNeeded` blocks at `NextFree`, header `NextFree` advances; the record stores the new block number as LE u32 |
| `OpenTable` | New `File`; `ReadHeader` (30B), `ReadColumns` (until `0x0D`); code page interpreted/validated; FPT header read when flagged | Nothing written; open fails with `ErrTableTruncated` when the file ends before `FirstRow + RowsCount·RowLength` | FPT header parsed; no blocks read until a memo field is interpreted |
| `GoTo(n)`/`Skip(k)` | Only `rowPointer` changes; clamped to `[0, RowsCount]`; beyond end sets pointer to `RowsCount` and returns `ErrEOF` | Nothing | Nothing |
| `Row()`/`Next()` | Reads `RowLength` bytes at pointer; `Interpret` per column; `Next` advances pointer only on success | Nothing (`Seek` moves the OS handle offset; every call seeks absolutely first) | Memo columns dereference the LE u32 pointer and read the entry on demand |
| `row.Write()` (existing, `Position ≤ RowsCount`) | `RowsCount` unchanged | Record bytes overwritten in place at `FirstRow + Position·RowLength` | Non-empty memo with the same pointer is appended as a new entry and the pointer updated; the old block becomes unreferenced (see orphan note) |
| Logical delete | `row.Deleted = true` then `row.Write()` | Marker byte of that record rewritten from `0x20` to `0x2A`; `RowsCount` unchanged; all other bytes stay | Unchanged; memo blocks are not reclaimed |
| Physical removal (PACK) | **Not implemented** in this version. The library never rewrites or shrinks the file; deleted rows remain physically present and are only filtered via `Deleted()`/`Rows(..., skipDeleted=true)` | Marker `0x2A` only | Unchanged |
| `Close()` | Marks `closed=true` after waiting for in-flight locked operations; subsequent locked entry points return `ErrClosed`; repeat close is a no-op | OS handles closed; no flush of unsaved `Row` values (a `Row` is only durable after `Write`) | FPT handle closed |

## Failure boundaries and recovery

- **Append ordering** (`WriteRow` in every IO): the record bytes are written
  first; `RowsCount` is committed in the header only afterwards. A failure
  before the header write leaves tail bytes that no record references; memory
  (`header.RowsCount`, pointer) and a reopen still agree. The previous order
  (count first) could leave the table declaring a missing row.
- **Failed read**: `Row()`/`Next()` do not advance the pointer on error, and
  every operation starts with an absolute `Seek`, so a failed read leaves no
  poisoned OS file offset; retrying from the same pointer works.
- **Encoding failure**: charset decode errors wrap `ErrInvalidEncoding`
  (character and memo fields), distinct from layout errors.
- **Truncated record / oversized header count**: a short record read wraps
  `ErrRowTruncated`; a file smaller than the header declared size fails open
  with `ErrTableTruncated`; an unknown delete marker wraps `ErrInvalidMarker`.
- **Memo pointers**: pointer `< firstDataBlock` or beyond the physical FPT end
  wraps `ErrMemoOutOfBounds`; a pointer ≥ `NextFree` but inside the file wraps
  `ErrMemoFree` (points into unwritten/free space). A zero pointer means empty
  memo and is not an error.
- **Memo rewrite orphans**: overwriting a memo value appends a new entry (FPT is
  append-only here); the old block remains on disk but no DBF pointer
  references it, and `NextFree` never shrinks. There is no garbage collection /
  PACK for FPT blocks in this version.
- **Growing a memo in place past its old block span** can overwrite the
  following entry on disk because the writer reuses the supplied block number;
  callers that enlarge a memo should append instead of reusing a pointer (the
  default `WriteRow` path appends when the field has no prior pointer).

## Complexity

- `GoTo`/`Skip`/`EOF`/`Pointer`: O(1), memory only.
- `Row`/`Next`: O(RowLength) plus one FPT read per non-empty memo field; seek is
  O(1) on regular files.
- `Write`/`Add`: O(RowLength) plus FPT append for changed memo fields; one
  header rewrite (30 bytes) per appended record.
- `Search`: O(RowsCount · matched field length); memo fields are unsupported.
- `Rows`: O(RowsCount · RowLength) full scan; saves and restores the caller
  pointer.
- Memory per read is O(RowLength) plus decoded memo payloads.

## Compatibility notes

- Default FPT block size for newly created tables is 64 (Visual FoxPro
  convention); a stored size of 0 on legacy files is honored as one block per
  entry.
- The `0x1A` EOF byte is not appended by the writer; reads use the header
  declared count, not an end marker.
- Code page mark byte 29 is only validated when `Config.ValidateCodePage` is
  set; otherwise the converter supplied in the config wins, or the mark is
  interpreted when `InterpretCodePage` is set / no converter given.
