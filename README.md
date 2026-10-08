# tapemgr

tapemgr puts files on LTO tape and makes sure they can be read back.

Copying files to a tape with `cp` gives you no proof that what is on the tape matches
what you wanted to keep. tapemgr calculates a checksum of every file while it writes it,
stores those checksums on the tape itself, and later reads everything back to confirm
the tape still holds the exact same data. Only then do you know it is safe to delete
the originals.

It works with any LTO drive through [LTFS](https://github.com/LinearTapeFileSystem/ltfs).

## What it does

- **Archives** files and folders to tape, one after another, so the drive keeps streaming.
- **Checks** every file by reading it back from tape and comparing checksums.
- **Survives interruptions.** If the network drops or the machine reboots in the middle
  of a 200 GB file, run the same command again and it continues where it stopped.
- **Remembers your tapes.** A local catalog lets you search for a file and see which
  tape it is on, without inserting every cartridge.
- **Skips duplicates.** A file that is already on one of your tapes is not written again,
  so archiving the same folder later only adds what is new.
- **Keeps a second copy** on another tape if you want one (`--copies 2`), so losing a
  whole cartridge loses nothing. Originals can then be deleted only once both copies
  passed a check.
- **Pinpoints damage.** If part of a file goes bad, it tells you exactly which bytes.
- **Repairs damage.** About 10% extra recovery data is stored with every file. If a
  scratched or creased part of the tape becomes unreadable, the file can still be
  restored exactly.
- **Watches the drive.** It reads the drive's own health reports and warns you when it
  asks for a cleaning cartridge or reports read or write errors.
- **Works with drive encryption.** LTO drives can encrypt everything with AES-256 at
  full speed. tapemgr creates keys for LTFS, shows whether the drive is encrypting, and
  can refuse to write unless it is (`--require-encryption`). A tape that was written
  encrypted never gets unencrypted data added.
- **Encrypts files if you want** (`--encrypt-to`), in the standard
  [age](https://age-encryption.org) format, for drives without hardware encryption or
  if you'd rather not depend on one. The archiving machine only needs your public key,
  and any age tool can decrypt the files, even without tapemgr. Tapes can still be
  checked and repaired without the key.
- **Deletes originals safely.** Only files that this machine archived and that passed a
  check on tape are deleted, and only after you confirm.

Every tape describes itself. Even without tapemgr or the catalog, you can check a tape
with `sha256sum -c SHA256SUMS`.

## Install

Requires Go 1.27 or newer.

```bash
go install github.com/Knight1/tapemanager/cmd/tapemgr@latest
```

## Quick start

```bash
# Prepare and mount the tape (LTFS tools, once per tape)
sudo mkltfs -d /dev/st0 -n NAS-2026-001
sudo ltfs /mnt/ltfs -o devname=/dev/st0

# Archive a folder
tapemgr archive put --label NAS-2026-001 /mnt/nas/downloads/

# Read it all back and check it
tapemgr archive verify

# Only now: delete the originals (asks for confirmation)
tapemgr archive purge-source /mnt/nas/downloads/

# Done: unmount and eject
sudo umount /mnt/ltfs
tapemgr drive eject
```

For a second copy, insert another tape and run:

```bash
tapemgr archive put --copies 2 /mnt/nas/downloads/
tapemgr archive verify
tapemgr archive purge-source --copies 2 /mnt/nas/downloads/
```

Later, find a file without loading any tape:

```bash
tapemgr catalog search ubuntu
```

## Commands

| Command | What it does |
| --- | --- |
| `tapemgr archive put <folder>` | Write a file or folder to the tape |
| `tapemgr archive verify` | Read everything back and check it |
| `tapemgr archive list` | Show what is on the mounted tape |
| `tapemgr archive purge-source <folder>` | Delete originals that are safely on a checked tape |
| `tapemgr archive restore --to <dir> [path]` | Copy files back from tape, repairing damage on the way |
| `tapemgr archive recover` | Add files to the tape's records that are missing from them, for example on tapes filled with `cp` |
| `tapemgr catalog tapes` | Show all known tapes and when they were last checked |
| `tapemgr catalog search <text>` | Find files across all tapes |
| `tapemgr catalog import` | Add an existing tape to the catalog |
| `tapemgr archive keygen --out <file>` | Create a key pair for `put --encrypt-to` and `restore --identity` |
| `tapemgr catalog retire <tape>` | Mark a lost or failing tape, so its files get copied again |
| `tapemgr archive repair-volume` | Rebuild a tape's damaged or missing identity record from the catalog (asks first) |
| `tapemgr drive info` | Show the drive, the loaded cartridge, its free space and error counts |
| `tapemgr drive check` | Warn if the drive needs cleaning or reports errors (good for cron) |
| `tapemgr drive selftest` | Run the drive's built-in self-test and report whether it passed (`--extended` for the long one, `--status` to read the last result) |
| `tapemgr drive density` | Show which LTO generations the drive can read and write, and whether hardware compression is on |
| `tapemgr drive load` / `eject` | Load or eject the cartridge (eject is refused while the tape is mounted) |
| `tapemgr drive log` | Show and explain the drive's error history |
| `tapemgr drive firmware --file <image>` | Update the drive's firmware (checks the image's checksums and IBM signatures first, `--skip-image-check` skips that, then asks for the drive's serial number) |
| `tapemgr drive inspect-firmware --file <image>` | Show an IBM image's sections, checksums and signatures, without a drive |
| `tapemgr drive list` | Show the tape drives attached to this machine |
| `tapemgr drive keygen --out <file>` | Create an encryption key file for LTFS; `--append` adds a key to an existing one (see [drive encryption](docs/TECHNICAL.md#drive-encryption)) |

The tape is expected at `/mnt/ltfs` and the catalog at `/var/lib/tapemgr`. Use
`--tape` and `--catalog` to change that.

## Status

Young but usable. Planned next: optional compression before age encryption, and
filling the rest of a full tape with whole folders that still fit.

Tested with Debian 13, an IBM ULT3580-HH6 (LTO-6) drive and LTFS 2.4.9.

For file formats, the resume mechanism and other details, see
[docs/TECHNICAL.md](docs/TECHNICAL.md).

## License

MIT, see [LICENSE](LICENSE).
