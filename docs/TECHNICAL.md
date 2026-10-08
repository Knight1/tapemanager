# Technical notes

## Write path

```text
source file ─► 4 MiB chunks ─┬─► LTFS file (<name>.tapemgr-partial)
                             ├─► SHA-256 of the whole file
                             └─► SHA-256 of each chunk
```

- Files are written strictly one at a time, in lexical order. No parallel tape writes.
- Each source byte is read once. File and chunk hashes come from the bytes written.
- A file is written as `<name>.tapemgr-partial`. After that it is synced, given the
  source mtime, and renamed. Then its manifest record goes to the local pending log.
- A source file that changes size or mtime during the copy is rejected.

## On-tape layout

```text
/
├── <your files>
├── SHA256SUMS                          sha256sum -c compatible, real files only
└── .tapemgr/
    ├── volume.json                     {"id": UUID, "label", "ltfs_uuid", "created"}
    └── segments/
        ├── 000001.manifest.jsonl       one entry per file, authoritative
        ├── 000001.chunks.jsonl         chunk hashes for those files
        ├── 000001.parity               Reed-Solomon parity data
        ├── 000001.parity.jsonl         parity layout per file
        └── 000002.…
```

Manifest entry:

```json
{"path":"downloads/foo.iso","size":200987345920,"sha256":"5e8f…","mtime":"…","source":"/mnt/nas/downloads/foo.iso","archived_at":"…"}
```

A deduplicated entry has `"ref":{"tape":"<volume id>","path":"<path>"}` and no data
on this tape.

## Metadata segments

Tape is append-only. Every write lands at the end of the recorded data, even a write
to an existing file, so space can't be reserved for metadata. Appending one manifest
line per file would leave the manifest in thousands of small pieces between the data
files. Reading it back would then need one seek per piece, and each piece would also
enlarge the LTFS index.

Instead:

1. When a file is complete, its record goes to a local pending log
   (`<catalog>/pending/<volume id>.jsonl`).
2. Every 100 GiB, and at the end of every run (also a failed one), the pending
   records go to the tape as a new segment. Each segment file is written in one
   piece, so it stays contiguous. `SHA256SUMS` is rewritten the same way.
3. Write order: chunks file, manifest file, `SHA256SUMS`, parity data, parity list,
   then the pending log is cleared. A segment counts once its manifest file exists.
   Parity comes last because it is large and optional. If it fails (for example
   on a full tape), the files stay recorded and only lose their parity, with a
   warning. Leftovers of an aborted attempt at the same segment number are removed
   before writing.

   If the process dies during the parity write, the manifest is already on tape.
   The next run finds those records already recorded and writes their still-staged
   parity into the same segment as an addendum, so the parity isn't lost. A stale
   `SHA256SUMS` (crash between manifest and checksum file) is rewritten at the start
   of the next run.

A full LTO-6 ends up with about 25 segments. Superseded copies of `SHA256SUMS` stay
on tape as dead space, a few MB each.

If a run dies before flushing, the next `put` writes the leftover records first
(`RECOVERING: …`). Records that already reached the tape are skipped, so nothing is
written twice. `verify` warns while records are still pending.

If the local pending log is lost as well, the files of that unflushed batch are on
tape but not in its manifest. `put` refuses to overwrite them, and
`archive recover` records them.

## Tape full and multiple tapes

Before each file, `put` checks free space on the tape. The file, its parity, all
parity staged for the next segment, and a metadata reserve (256 MiB plus an estimate
for the manifest, chunk lists and `SHA256SUMS`) must fit. If they don't, `put` stops
before the file with "tape is full". The final segment write then still fits. The
catalog disk must have room for the file's staged parity.

Before checking, the part of the file that a resume will reuse is subtracted, but
only that part: LTFS never reclaims space, so rewriting a partial file needs its full
size again.

When `put` stops because the tape is full, run the same command on the next tape.
Files that are already on other tapes are skipped (see Copies below), so it continues
where it stopped.

## Resume

Every 1 GiB, tapemgr syncs the file to tape and appends a checkpoint to a local
journal. A checkpoint holds the offset, the serialized SHA-256 state and the new
chunk hashes. Journals live at `<catalog>/journal/<volume id>/<hash of path>.jsonl`.

On restart, for a file with a partial on tape and a matching journal (same source
path, size, mtime, change ID and chunk size):

1. Take the newest checkpoint whose offset is not beyond the partial file's size.
   LTFS can lose data written after its last index update, so the partial may be
   shorter than the newest checkpoint.
2. Read back the last chunk before that offset and compare it with its hash. If it
   does not match, try the previous checkpoint.
3. Truncate the partial to the offset, restore the hash state, seek the source,
   continue.

The change ID is the source's device, inode and ctime. Unlike the mtime, the ctime
cannot be set by users and changes with every write, so a source rewritten with its
old mtime kept (`rsync -t`, `touch -r`) starts over instead of being resumed. Resuming
it would mix two versions in one file, and for encrypted files would encrypt new
content under a key and nonce already used on tape. Journals of files already in the
tape manifest (left by a crash right after recording) are deleted at the start of the
next `archive put`, together with their staged parity.

If the journal says a file is complete but the manifest entry is missing (crash
between rename and append), the entry is written from the journal without
rewriting data.

## Verify

Reads files in manifest order (write order) to avoid repositioning. Each chunk is
compared to its recorded chunk hash, so damage is reported as byte ranges. A full verify
(no path filter) is recorded in the catalog.

## Deduplication

On by default (`--no-dedup` turns it off). For a source file of at least 1 MiB whose
size matches a known file, the source is hashed first. If the SHA-256 is already in
the catalog or on the current tape, only a reference entry is written. This costs an
extra read of the source, but only when sizes match.

## Purge

`archive purge-source` deletes a source file only if all of these hold:

1. This machine archived it. Records come from `<catalog>/written/`, never from a
   tape manifest, because a tape can claim any source path.
2. Its size and mtime still match that record (`--rehash` also compares SHA-256).
3. The cataloged tape manifest lists the same SHA-256 at the recorded location.
   For deduplicated files, the reference target must match too.
4. The tape holding the content passed its most recent full verification, and that
   verification happened after the file was archived. A later failed verification
   revokes earlier passes.

Before each deletion the file is checked again (same inode, size, mtime). All access
goes through an `os.Root` at the source directory. Directories left empty are removed,
except the source root itself. Paths that overlap the tape mount are refused.

Exit code 1 if anything was kept, the user aborted, or nothing qualified.

## Recover

`archive recover` hashes regular files on the tape that have no manifest entry and
adds them as one or more segments, with `"recovered": true` and no source. It reads
files in LTFS start block order (`user.ltfs.startblock`) to avoid seeking. It skips
`.tapemgr/`, `SHA256SUMS`, partial files and symlinks, and refuses to run while
records for that tape are pending. It also creates a volume record for tapes never
used with tapemgr. Recovered files can't be purged, because their source is unknown.

## Catalog

```text
/var/lib/tapemgr/
├── tapes/<id>.json      tape record and verification history
├── tapes/<id>.jsonl     copy of the tape's manifest (all segments)
├── pending/<id>.jsonl   records not yet written to the tape
├── written/<id>.jsonl   records this machine wrote (trusted by purge)
├── parity/<id>/         parity staged until the next segment write
└── journal/<id>/        resume journals
```

The catalog is updated after each `put` (also after a failed one) and each full
`verify`. `catalog import` rebuilds a tape's entry from the tape. Losing the catalog
loses verification history and the `written/` records, so files can no longer be
purged until they are archived again.

## Parity

`put --parity N` (default 10, maximum 25, 0 turns it off) stores Reed-Solomon parity
(`github.com/klauspost/reedsolomon`) for every file.

LTO already corrects random bit errors. It can't correct a damaged area of tape,
which shows up as a few MB of read errors. Parity targets that case. Chunk hashes
tell exactly which pieces are lost, so each parity piece rebuilds one lost piece.

| Layout | Used for | Data pieces | Organization |
| --- | --- | --- | --- |
| `rs-window` | files of 20 chunks (80 MiB) or more | the 4 MiB chunks | windows of 20 × 16 chunks; chunk *i* of a window goes to stripe *i mod 16* |
| `rs-small` | smaller files | 20 equal pieces | one stripe, piece hashes stored with the parity |

With `M = ceil(20 × N / 100)` parity pieces per stripe, interleaving makes one
contiguous damaged area of up to `M × 16` chunks per window repairable: 128 MiB per
1.25 GiB window at 10%. Overhead is `M / 20` for files of any size.

Parity is computed while streaming (memory: `16 × M × 4 MiB`, 128 MiB at 10%), staged
locally in `<catalog>/parity/<id>/`, and written with the next segment:

```text
.tapemgr/segments/NNNNNN.parity        parity data of all files in the segment
.tapemgr/segments/NNNNNN.parity.jsonl  {"path","layout","offset","hashes","data_hashes"}
```

Every parity piece has its own SHA-256. Damaged parity is treated as missing, and a
rebuilt chunk is accepted only if it matches its chunk hash. Parity can't produce
wrong data.

With parity on, resume checkpoints fall on window boundaries, so no encoder state
needs saving. Small files are buffered for their parity and restart instead of
resuming.

`verify` keeps reading past read errors and sorts each file as OK, repairable or
failed. A full verify also reads each segment's parity data right after that
segment's files and checks every parity piece. Damaged metadata lines (manifest,
chunk lists, parity lists) are skipped and counted as problems, so one bad spot
doesn't hide the rest of the tape. `restore` reads metadata the same way.

Repairable damage, damaged parity and damaged metadata all fail the verification
(exit 1) and block `purge-source`: the tape is degrading and should not stay the
only copy. A full verify that can't finish at all is recorded as failed, so an
older passing verification can't keep allowing purges. If the tape's volume record
is damaged, the tape is identified in the catalog by its LTFS volume UUID or its
manifest entries, and the result is recorded there. If it can't be identified,
verify says so and fails.

`restore --to DIR [path]` writes good chunks as they are read, rebuilds damaged
ones one stripe at a time (memory stays at one stripe however much is damaged),
checks the result against the file's SHA-256, and only then moves it into place. It
never overwrites existing files and never touches files it didn't create: it writes
to a temporary name unique to the attempt (`<name>.tapemgr-restore-<random>`), then
uses a hard link, which fails atomically if the name is taken. On filesystems without hard links (FAT, exFAT), it checks right
before renaming instead. A file shorter than recorded counts as one missing range.

## Performance

Reads run ahead in their own goroutine. For each chunk, the tape write, the file
hash, the chunk hash and the parity update run in parallel. On a Xeon Gold 6138
(no SHA instructions, SHA-256 about 380 MB/s per core), a 3 GiB file copies at about
290 MB/s without parity and about 220 MB/s with 10% parity. Both are above LTO-6's
160 MB/s.

## Copies

Parity inside a tape doesn't help when the whole cartridge is lost. For that, each file
can be kept on several tapes, written with a single drive by running `put` again on
another tape. The sources are read again from disk.

Copies are counted per file, not by pairing tapes, so copies may fill up at different
points and still line up:

- `put --copies N` (default 1) skips a file when it already has N copies. The copies
  are the active tapes holding its SHA-256 as data in the catalog, plus this tape if
  it holds that content. The SHA-256 comes from this machine's record of the earlier
  write (`<catalog>/written/`), matched by source path, size and mtime, so the file is
  not read again. A run is therefore incremental: new files go to the new tape, the
  rest is skipped. `--again` skips nothing that isn't already on this tape.
- A tape counts once, however many times it holds the content. A copy run on the tape
  that holds the first copy adds nothing.
- A deduplication reference to another tape is no extra copy. It is only used when
  the content already has N copies. Otherwise the data is written.
- `purge-source --copies N` needs N different tapes holding the content, each listed
  in this machine's written records (directly or through a reference), each passing
  verification since the write, all with the same SHA-256.
- `catalog retire <tape>` marks a lost or failing tape. Its content no longer counts,
  so the next `put --copies N` writes those files again (while the source still
  exists), dedup never references it, and purge ignores it. `--undo` reverses it.
  Search still lists it, marked retired.
- `restore` names the other tapes holding a copy of each file it cannot restore.

A tape someone else wrote and that was imported into the catalog counts as a copy for
`put`'s skip decision, but never for purge, which trusts only this machine's records.

Parity across several tapes (RAID-like sets) was considered and rejected. Rebuilding
a lost tape would need every other tape of the set. With one drive that means
loading each tape in turn, plus terabytes of local staging. Copies give simpler
recovery with far less code.

## Planned: keeping related files together

When the next file doesn't fit, `put` currently stops and asks for the next tape. Filling
the rest of the tape with later, smaller files would scatter related files (an ISO and
its README) across tapes. The idea, not yet decided:

- The unit of placement is a group: by default the files directly in one folder, or
  whole subtrees with `--group-depth N`. Loose files sharing a name stem
  (`ubuntu.iso`, `ubuntu.iso.sha256`) form one group.
- Gaps are filled only with whole groups that fit. A group is split only if it is
  larger than a whole tape, at file boundaries and with a warning.
- `--stop-when-full` keeps today's strict order.
- The catalog records the group, so restore can say where the rest of a folder is.
- `archive plan <source>` shows the tapes needed and what goes on each, including
  parity and metadata, without writing.

Open question: should the default group be each folder (fills tapes better) or each
top-level folder under the source (keeps projects together, leaves bigger gaps)?

## Drive access

`tapemgr drive` talks to the drive directly with SCSI commands through the Linux SCSI
generic interface (`SG_IO` on `/dev/sgN`). No external tools are run. The sg node stays
usable while LTFS holds `/dev/st0`, so the queries work with a mounted tape.

| Command | Source |
| --- | --- |
| Identity | INQUIRY, VPD page 0x80 (serial) |
| Ready, cartridge present | TEST UNIT READY (unit attentions are retried) |
| Cleaning request, activity | LOG SENSE 0x11, VHF data (CRQST, CRQRD) |
| TapeAlert flags | LOG SENSE 0x2E |
| Drive error counters | LOG SENSE 0x02 (write), 0x03 (read), 0x06 (non-medium errors) |
| Drive lifetime statistics | LOG SENSE 0x14: loads, cleanings, power-on and tape motion hours, meters of tape, power cycles, hard errors, hours per cartridge type |
| Compression | LOG SENSE 0x1B: enabled, read and write ratio and bytes since the cartridge was loaded |
| Cleaning required | LOG SENSE 0x0C (sequential access), in addition to the VHF bits |
| Cartridge statistics | LOG SENSE 0x17 (volume statistics): mounts, passes, retries and unrecovered errors over its life and the last mount, MB written and read during the last mount, native capacity and use |
| Cartridge details | READ ATTRIBUTE (MAM) partitions 0 and 1: serial, maker, date, density, load count, capacity, lifetime MiB, LTFS volume UUID from the coherency attribute |
| Error history | LOG SENSE 0x16 (tape diagnostic data): the drive's last failed operations, one slot each |
| Write protection | MODE SENSE header (WP bit), or VHF data |
| Firmware build | VPD page 0xC0 on IBM drives: firmware name, build date and time, platform |
| Firmware update | READ BUFFER mode 03h (buffer descriptor), WRITE BUFFER mode 07h (download microcode with offsets and save) |
| Load, eject | LOAD UNLOAD |

`drive info` prints two sections, the drive first and the loaded tape second, so
counters of the drive and of the cartridge are never mixed.

### Error log

`drive log` lists the used slots of the tape diagnostic data page: what failed
(sense key and additional sense code), during which command, with which cartridge,
under which firmware level, and when (most drives count milliseconds from power-on,
so times are only comparable within one power cycle). The number of slots varies
by drive. The analysis groups medium errors by cartridge, flags errors on the loaded
cartridge, points at the drive when errors spread over three or more cartridges,
reports hardware errors, and notes entries recorded under older firmware. Medium
errors on the loaded cartridge are also a warning in `drive info` and `drive check`.

### Firmware update

`drive firmware --file IMAGE` downloads a vendor firmware image with the standard
SPC procedure: WRITE BUFFER mode 07h in 256 KiB pieces (rounded to the drive's offset
boundary). Some firmware levels report an offset boundary byte the standard does not
define (IBM LTO-6 H991 and later report 0x86); the 256 KiB pieces then keep their
alignment, which suits any boundary a drive can sensibly require. The drive activates the image only once it is complete, so an interrupted
download leaves the old firmware running. Before sending, it requires:

- a tape drive (INQUIRY), no LTFS mount on it, and no cartridge in it. After an eject the
  cartridge stays in the slot and the drive reports "load needed" (ASC 04h/02h) until it
  is taken out; `drive info` shows that state;
- an image between 64 KiB and 16 MiB, the limit of WRITE BUFFER's offset field.
  The capacity the drive reports for buffer 0 (5 MiB on the IBM LTO-6) is not a
  limit: IBM LTO-6 images are about 6 MiB;
- on IBM drives, an image built for this drive: the image header (magic
  `IBMTpDrv`) carries its length, the load ID, the firmware level and an EBCDIC
  model ID in the same layout as the drive's VPD page 0x03. Length and file size
  must agree, and load ID and model ID must match the drive. Files without that
  header are refused. Other vendors' images are not checked here; the drive checks
  them itself;
- for IBM images, the drive's own checks (below), run right after the file is read
  and before the drive is opened, so a damaged or changed file is refused at once.
  It runs silently: the summary shows one `Checks:` line, and a failure lists every
  failed check. `--skip-image-check` skips it, which the summary shows before the
  serial number is asked for; the drive still checks the image itself;
- on IBM drives, an image whose interface and form factor (attribute 9, for example
  `sas_hh`) match what the drive reports in VPD page 0xC0, also with
  `--skip-image-check`;
- the drive's serial number typed as confirmation (or `--yes`).

#### IBM image checks

IBM images (an embedded Linux, not encrypted) carry four layers of checks, and the
drive refuses an image that fails any of them. tapemgr runs the same checks before
sending, and `drive inspect-firmware --file IMAGE` shows them without a drive:

1. **Container:** magic `IBMTpDrv` at 0x20, the length at 0x04 equal to the file
   size, and a length that is a multiple of 4. All integers are big-endian.
2. **Sections:** a table (offset at 0x38, size at 0x3c, 32 bytes per entry: name,
   flags, file offset, size, load address, memory size) lists the sections. The
   top byte of the flags names the checksums the section must carry, exactly those.
   They follow its data as descriptor and value pairs ending with a zero word.
   A descriptor holds a tag in its top byte and the value length in its low 12 bits.
   Tag 0x80 is the sum of the section's 32-bit words, tag 0x40 a byte-wise CRC whose
   table entry for byte `i` is `gmul(i,0x38)<<24 | gmul(i,0xcf)<<16 | gmul(i,0x38)<<8 | i`
   (multiplication in GF(2^8) with polynomial 0x11d).
3. **Whole image:** a chain of records read backwards from the last word, each value
   stored right before its descriptor, ending at a zero word. The word sum (0x80) and
   CRC (0x40) cover the image from offset 0 to where their value is stored.
4. **Signatures:** RSA-2048 PKCS#1 v1.5 records in the same chain, covering the image
   from offset 0 to where the signature is stored: tag 0x20 over SHA-1, checked with
   the certificate `CN=L4H_FIRMWARE`, and tag 0x10 over SHA-256, checked with
   `CN=TAPEFIRMWARE`. The SHA-1 signature may instead cover the image from 0x20. A CRC
   and a SHA-256 signature are mandatory, so removing a signature does not help.

The two certificates (`internal/ibmfw/certs`, public keys only) are IBM's and are built
into tapemgr: a key taken from the image under test would prove nothing. Their SHA-256
fingerprints are fixed in the tests. Like the drive, tapemgr uses the keys directly,
without a certificate chain and without the validity dates: `L4H_FIRMWARE` expired in
June 2026 and still signs current images. Images for other drive generations may be
signed with other keys and are then refused.

The last records of the chain, the SHA-256 descriptor and two 32-byte exclusion lists
(tags 0xf0/0x90 and 0xf1/0x91, a set bit excludes one drive type), are not covered by
any signature; `inspect-firmware` shows where the signed part ends. The hardware ID
range at 0x30 to 0x34 is shown but not checked, since the drive's own ID is not
readable from outside. Every offset and length in an image is checked against the file
before use; the parser is fuzzed.

It prints the image's SHA-256 to compare with the vendor's, and for IBM images
the image's level and build date. `--dry-run` runs every
check and sends nothing. Afterwards it waits up to 15 minutes for the drive to
restart and reports the old and new firmware level. tapemgr does not download
images; get them from the drive vendor (for IBM drives sold by Lenovo, see the
vendor's microcode page for the drive). Tried on an IBM LTO-6 half-height SAS drive
from E6R3 over H991 to KAJ1. While the drive saves the last piece it does not answer,
often until after its restart, so the progress stops just below 100% for minutes.

The installed firmware image cannot be read back for a backup: on the IBM LTO-6 the
READ BUFFER data buffers hold a copy of the cartridge memory and drive data, not the
microcode. Keep the vendor's image files instead. `drive info` shows the firmware
level and, on IBM drives, its build (VPD 0xC0).

The drive is chosen by `--device` (or `$TAPEMGR_DEVICE`), else the drive behind the
LTFS mount at `--tape` (found through `/proc/mounts` and `/sys/class/scsi_tape`), else the
only drive attached. A tape node such as `/dev/nst0` is mapped to its sg node.

After `archive put` and `archive verify`, the drive behind the mount is checked and
problems are printed as `DRIVE WARNING`. Most drives clear TapeAlert flags once they
are read, so a flag shown there will not show again in a later `drive check`.

Safety:

- Commands that move media are only sent after INQUIRY confirms a tape drive, since
  the same opcode stops a disk.
- Eject is refused while an LTFS mount uses the drive (matched by device node,
  `/dev/tape/by-id` link or drive serial). For a device unknown to sysfs, any LTFS
  mount blocks it. LTFS also locks the cartridge while mounted, and the drive's refusal
  is reported.
- Every length in a drive response is checked against the bytes received; the
  parsers are fuzzed.
- Set `TAPEMGR_TEST_DEVICE=/dev/sg2` to run the read-only queries against a real drive
  in `go test`.

## Drive encryption

LTO-4 and newer drives encrypt in hardware with AES-256-GCM, as defined by the LTO
format, so any LTO drive that can read the tape's generation and has the key can
decrypt it. The drive compresses before it encrypts, so hardware compression and
full speed are kept. Everything on the tape is encrypted, including tapemgr's
metadata and the LTFS index. The 256-bit key is never stored on the tape, only a
12-byte key ID.

LTFS loads the keys, because it must read its own index: its `flatfile` key manager
takes a key file of alternating `DK=` (base64 key) and `DKi=` (key ID: three
characters and 18 hex digits) lines. tapemgr does not set keys itself, which would
conflict with LTFS.

```bash
tapemgr drive keygen --out /etc/tapemgr/ltfs-keys   # prints the key ID, never the key
# more keys in the same file: add --append
mkltfs -d /dev/st0 -n LABEL --kmi-backend=flatfile \
    -o kmi_dk_list=/etc/tapemgr/ltfs-keys -o kmi_dki_for_format=<key ID>
ltfs /mnt/ltfs -o devname=/dev/st0 -o kmi_backend=flatfile \
    -o kmi_dk_list=/etc/tapemgr/ltfs-keys
```

`keygen` adds to the file through a temporary file created with mode 0600 and never
overwrites existing keys. It refuses a key file other users can read, and symlinks.
Losing the key file loses every tape encrypted with it, so back it up away from the
machine.

tapemgr reads the drive's state with SECURITY PROTOCOL IN (tape data encryption
pages 0x0010 capabilities and 0x0020 status):

- `drive info` shows whether the drive is encrypting, the algorithm and the key ID,
  or which algorithm the drive supports for the loaded cartridge.
- `put --require-encryption` and `recover --require-encryption` (or
  `TAPEMGR_REQUIRE_ENCRYPTION=1`) refuse to write unless the drive behind the mount
  reports that it is encrypting. A mount without the key options therefore never
  writes plain data unnoticed.
- After writing while the drive encrypts, the catalog records the tape as encrypted
  with the key IDs used (`catalog tapes` shows them). From then on, unencrypted data
  is never added to that tape, with or without the flag: writes are refused if the
  drive is not encrypting or its state can't be read. A different key is allowed,
  with a note that both keys are now needed.

Not yet verified with a real encrypted write on the test drive. Some IBM drives must
be configured for application-managed encryption before they accept keys from LTFS.

## age encryption

An alternative to drive encryption, per file and independent of the drive, LTFS
and tapemgr: `put --encrypt-to <public key>` (repeatable, or `--encrypt-to-file`,
default `$TAPEMGR_ENCRYPT_TO_FILE`) stores each file as `<path>.age`, a standard
[age](https://age-encryption.org) file. Without the flag nothing changes.

```bash
tapemgr archive keygen --out ~/tape-key.txt          # prints the public key
tapemgr archive put --encrypt-to age1... /mnt/nas/photos/
tapemgr archive restore --identity ~/tape-key.txt --to /restore
age -d -i ~/tape-key.txt photo.jpg.age > photo.jpg   # without tapemgr
```

- The archiving machine needs only the public key. A recipients file containing a
  private key is refused.
- Several recipients are possible, for example an everyday key plus an offline
  emergency key. Post-quantum keys (`keygen --post-quantum`, `age1pq1...`) work too,
  but age does not allow mixing them with classic keys.
- File names, sizes and folder structure stay visible; only contents are encrypted.
- No software compression: encrypted data can't be compressed by the drive either,
  so these files take their full size.

How it is written: the age library writes the header (file key wrapped for each
recipient, header MAC). The payload (ChaCha20-Poly1305 over 64 KiB chunks, keyed
from the file key and a 16-byte nonce) is produced by `internal/agestream`, because
it is a pure function of the file key, the nonce and the data. That makes the
encrypted size known in advance, so the tape-full check and parity layout work as
before, and any part of the file can be regenerated for a mid-file resume. Tests
decrypt every produced file with the age library.

- The manifest entry keeps the original path, size and SHA-256 (used for dedup,
  copies and purge) and adds an `age` block with the encrypted file's size, SHA-256
  and recipients. Chunk hashes, parity, `verify` and `SHA256SUMS` cover the encrypted
  file, so a tape can be checked, and repaired, without the key.
- `restore` rebuilds the encrypted file (with parity repair if needed), checks its
  SHA-256, decrypts it with the age library, and keeps the result only if size and
  SHA-256 match the original. `--identity` (repeatable, default `$TAPEMGR_IDENTITY`)
  names the private key files.
- Resume: the journal stores the age header, the nonce and the file key, plus the
  plaintext hash state at each checkpoint. Journals are mode 0600 and deleted once
  the file is recorded. Before continuing, the part of the current 64 KiB chunk that
  is already on tape is regenerated and compared. If the source changed, the write
  starts over with a new key, so two versions are never encrypted under the same key
  and nonce.
- `x` encrypted is stored as `x.age`; `put` refuses to write when that collides with
  an existing file of that name. Switching between encrypted and unencrypted runs
  never resumes the other kind of write.
- Throughput: about 165 MB/s on the test machine (Xeon Gold 6138), above the
  LTO-6 drive's native 160 MB/s. The plaintext SHA-256 is the limit.

## Crash safety

tapemgr assumes it can be stopped at any moment: host crash, power loss, OOM kill,
full disk or tape. The rules:

- **Replacing a file:** write to `<name>.tmp`, fsync, rename over the old file, fsync
  the directory. Readers always see the complete old or the complete new version.
  The old file is never deleted first, because that would leave a moment with no
  file at all.
- **Append-only logs** (pending, journal, written): one complete line per write, fsync
  after each. Readers drop a torn last line.
- **LTFS index:** LTFS writes its index to tape only every few minutes or at unmount,
  and after a host crash it rolls back to the last index. After writing a segment,
  tapemgr forces the index to tape by setting `user.ltfs.sync` on the mount root.
  Only then does it clear the local pending log, which until that moment is the only
  durable copy of the records. On an LTFS mount (one that reports
  `user.ltfs.volumeUUID`) every failure counts, including "not supported" (LTFS
  answers `EACCES` if it lacks the attribute): the records then stay pending and the
  next run retries.
- **Torn log tails:** a crash can cut off the newline of a complete last record.
  Before appending, tapemgr adds it, so the next record never joins that line.
- **Lost volume record:** a tape with tapemgr records but no readable
  `.tapemgr/volume.json` is refused by `put` and `recover`; giving it a new identity
  would make it a different tape to the catalog. `tapemgr archive repair-volume`
  rebuilds the record from the catalog, only when run by hand and confirmed. It
  identifies the tape by its LTFS volume UUID or its manifest records, refuses if the
  tape's records do not match the cataloged tape (or with no readable records, unless
  `--id` names the tape), keeps a damaged file as `volume.json.damaged`, and forces
  the LTFS index to tape.
- **Rolled-back files:** before recording a pending file in a segment, tapemgr checks
  that it is really on tape at full size. A file undone by an LTFS rollback is not
  recorded and gets archived again by the next run. Resume journals likewise use the
  newest checkpoint the partial file still covers.

## Safety

- Writes are refused unless the tape root is a FUSE mount (`--no-mount-check` skips
  this), so an unmounted `/mnt/ltfs` never fills the local disk.
- `put` and `recover` refuse a read-only tape before scanning or writing anything.
  The mount is checked with `statfs` (LTFS mounts a write-protected cartridge
  read-only), and the drive behind the mount is asked for its write-protect bit
  (MODE SENSE header, or VHF data), which gives the clearer message. If the drive
  can't be found or asked, the mount check alone decides.
- Everything read from a tape is untrusted. Paths must be clean, relative, and
  outside `.tapemgr/`. Volume IDs must be UUIDs, because they become file names.
  Hashes must be 64 hex characters. Chunk sizes must be between 16 B and 16 MiB.
  File sizes are capped at 1 PiB, so size arithmetic can't overflow.
  Parity layouts are bounded (at most 32 + 8 pieces of 16 MiB per stripe), so a
  crafted tape can't force large allocations during verify or restore.
- All tape file access goes through `os.Root`, which refuses paths and symlinks that
  lead outside the tape.
- **One lock** for all tapemgr commands on a host: `/run/lock/tapemgr.lock` (`flock`,
  opened without following symlinks). The kernel releases it when a process ends, also
  on a crash, so it never goes stale. Commands that write to a tape, move the drive,
  change its firmware or delete files (`put`, `recover`, `repair-volume`,
  `purge-source`, `catalog import`, `catalog retire`, `drive firmware`, `load`,
  `eject`) take it exclusively and run alone. Commands that only read a tape or query
  the drive (`verify`, `restore`, `list`, `drive info`, `log`, `check`) share it, so
  `drive info` still works during a long `verify`, but not during a `put`. Purely
  local commands (key generation, `catalog tapes`, `catalog search`, `drive list`,
  `drive inspect-firmware`) take none. A busy lock fails at once with a message; nothing waits. With several
  drives in one host this also serializes work on different drives.
- Key files are never replaced: `archive keygen` refuses an existing file, even one
  created while it runs, and `drive keygen` adds to an existing LTFS key file only
  with `--append`.
- **Memory check:** before a large allocation (reading a file with small-file parity,
  which is held whole; repairing a parity stripe; the parity state while archiving),
  tapemgr checks the free memory: the host's available RAM plus free swap, limited by
  every cgroup v2 memory limit above the process (systemd scopes, containers), with
  page cache counted as free. If it does not fit, the operation stops with a message
  instead of being killed by the kernel. `verify` then stops and records nothing,
  since a short machine says nothing about the tape; `restore` fails only that file;
  `put` stops before the file is started. The check is a snapshot, so it prevents
  most out-of-memory kills, not all.
- Names and labels from a tape can hold terminal escape sequences. All output goes
  through a filter that shows control characters as `\xNN` (newlines, tabs and the
  carriage returns of progress bars pass), and labels read from a tape have control
  characters replaced, so a crafted tape cannot rewrite the screen, set the
  clipboard or forge result lines. New labels with control characters are refused.
- Journal offsets are bounds-checked before use.
- A source file is reopened and compared with what was scanned, so a file swapped
  for a symlink after scanning is rejected.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Failure, verification mismatch, or no search results |
| 2 | Usage error |

## Development

```bash
gofmt -w . && go vet ./... && go test -race ./... && govulncheck ./...
```

govulncheck reports GO-2026-5932 at module level for `golang.org/x/crypto`: the
module contains the deprecated `openpgp` package. tapemgr (and age) only use its
ChaCha20-Poly1305 and HKDF packages and never import `openpgp`, so the finding does
not apply. Reviewed 2026-10-05.
