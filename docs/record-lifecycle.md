# Record lifecycle: memory, DBF bytes and FPT blocks

This document is an executable map of one dBase record. Every transition below
is exercised byte for byte by `TestRecordLifecycleByteFixture`
(`dbase/lifecycle_fixture_test.go`); concurrency and failure transitions are
covered by `lifecycle_concurrency_test.go` and
`lifecycle_diagnostics_test.go`.

All offsets are for the fixture used in the tests:

| item | value |
| --- | --- |
| version | FoxPro `0x30` |
| columns | `NAME C(10)`, `NUM N(5,0)`, `BIRTH D(8)`, `ACTIVE L(1)`, `NOTE M(4)` |
| `FirstRow` | `296 + 5*32 = 456` |
| `RowLength` | `1 + 10 + 5 + 8 + 1 + 4 = 29` (delete marker first) |
| FPT block size | `64`; header reserves the first `512/64 = 8` blocks |
| first data block | `NextFree = 8` |

## State containers

A `File` owns three layers that change independently:

1. **Memory**: `file.header` (counts, date, geometry), `file.table.rowPointer`
   (shared cursor), `file.table.columns`, and the transient `Row`/`Field`
   values returned to callers.
2. **DBF bytes**: the 32 byte header (30 used), column descriptors from offset
   32, the `0x0D` header terminator, then fixed length records beginning at
   `FirstRow`. Byte 0 of every record is the delete marker (`0x20` active,
   `0x2A` deleted).
3. **FPT blocks**: a fixed 512 byte header (`NextFree` big endian at 0, block
   size big endian at 6) followed by data blocks. Each data block begins with
   a 4 byte signature (`1` text, `0` binary) and a 4 byte big endian length.
   The record stores only the little endian block number in its 4 byte memo
   field; an all-zero pointer means "no memo".

## Lifecycle transitions

### Open

* `OpenTable` reads the DBF header and column descriptors into memory, derives
  columns and mods, optionally interprets the code page mark, and reads the FPT
  header when the memo table flag is set.
* No record bytes are cached: each `Row` is decoded on demand.
* The lifecycle lock starts unlocked and `closed=false`.

### Seek / GoTo / Skip (cursor only)

* `GoTo(n)` and `Skip(d)` only mutate `table.rowPointer`; no DBF/FPT bytes move.
* The physical file offset is **not** the cursor. Every read seeks from
  `FirstRow + rowPointer*RowLength` and restores nothing afterwards, so callers
  must not rely on the OS-level file offset.
* `Skip` clamps to `[0, RowsCount]`; negative offsets clamp to `0` without
  uint32 underflow.

### Row / ReadRowAt (decode)

* Seek to the record offset, read exactly `RowLength` bytes, inspect the delete
  marker, then run each field through `Interpret`.
* `Row`/`Next` use the shared cursor; `ReadRowAt(position)` is cursor free and
  is the entry point intended for concurrent readers.
* A memo field seeks to `block*BlockSize` in the FPT, reads signature/length,
  reads the payload and charset-decodes text memos.
* Empty (`nil`) values decode as: blank character -> spaces/empty string,
  blank numeric/date -> zero value, blank logical -> `false`, zero memo pointer
  -> empty `[]byte`.

### Write / Add (encode + commit)

Ordering matters and is the same on Unix, Windows and `GenericIO`:

1. `ToBytes` encodes every field. Memo values allocate (or reuse) FPT blocks
   and the returned 4 byte pointer is embedded in the record bytes.
2. For an append (`Position >= RowsCount`) the record offset is calculated as
   `FirstRow + (Position-1)*RowLength`; limits (`MaxRecordsPerTable`,
   `MaxTableFileSize`) are checked before any mutation.
3. Record bytes are written first.
4. Only then `RowsCount` is incremented in memory and the 30 byte DBF header
   is written last (header bytes 1-3 also receive the current date).

Therefore a failed record write never advertises a record that does not exist,
and a failed header write rolls the in-memory count back; the file stays
reopenable.

### Memo allocation

* New memos are appended at `NextFree`; the number of blocks is
  `ceil((8+payload)/BlockSize)` and `NextFree` advances by that amount before
  the block payload is written.
* Rewriting a record whose memo pointer is already set rewrites the existing
  block **in place**; `NextFree` does not move and no new block is allocated.
* An empty memo value keeps the pointer all zero and writes no block.

### Delete (soft)

* There is intentionally no physical `PACK`/purge operation in the package.
* Deleting sets `Row.Deleted=true` and writes the record with marker `0x2A`.
  Nothing else changes: `RowsCount`, record offsets, other record bytes and the
  FPT blocks all remain byte identical.
* `Rows(false, true)` filters marked rows; `Rows(false, false)` still returns
  them with `Deleted == true`.

### Close

* `Close` takes the lifecycle write lock, so it waits for every in-flight read
  or write, closes the DBF handle and then the FPT handle, and marks the file
  closed.
* After `Close`, all entry points fail with `ErrClosed` (`TABLE_CLOSED`),
  including a second `Close`. Pending data is not flushed beyond what each
  write already committed; there is no deferred buffer.

## Byte map (fixture, after one append)

```
DBF
0          0x30 version
1..3       last update YY MM DD
4..7       RowsCount (little endian) = 1
8..9       FirstRow = 456
10..11     RowLength = 29
12..27     reserved
28         table flags = 0x02 (memo)
29         code page = 0x03
32..32+32n column descriptors (32 bytes each)
456        record 0: 20 | NAME(10) | NUM(5) | BIRTH(8) | ACTIVE(1) | NOTE block LE(4)
485        record 1 begins here when appended
```

```
FPT (block size 64)
0..3       NextFree = 9 after one text memo "hello memo"
6..7       BlockSize = 64
0..511     header region (blocks 0..7, reserved)
512 (=8*64) 00 00 00 01 | 00 00 00 0A | "hello memo" | zero padding
```

## Complexity

| operation | time | extra memory |
| --- | --- | --- |
| open | O(columns) | header + descriptors |
| `ReadRowAt` | O(1) seeks + O(memo fields) FPT reads | one row |
| `Search` | O(records * matched field reads) | matched rows |
| append | one DBF write + O(memo) FPT writes | one row |
| in-place write | one DBF write; memo rewritten only when changed | one row |
| `Rows` | O(records) | all decoded rows |
| close | O(1) system calls | none |

## Compatibility notes

* New tables with memo columns default to block size 64 when 0 is passed to
  `NewTable`, and initialize `NextFree` to 8. Earlier code wrote the first memo
  at block 0 and overwrote the FPT header; files already created that way were
  not readable by external FoxPro tools. A block size of 512 (dBase IV style)
  still starts data at block 1.
* Reading existing FPT files is unchanged; validation is additive (see
  `architecture.md` diagnostics). Files whose memo pointers legitimately point
  into the 512 byte header region are not produced by FoxPro and are now
  rejected.
