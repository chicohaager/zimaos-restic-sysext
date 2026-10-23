# restic for ZimaOS

**Version 1.0.0** · ZimaOS 1.8.0 (amd64) · sysext module `restic`

[restic](https://restic.net), the backup program, as a native ZimaOS module — no container: it runs on the host and reads `/DATA` and the attached disks directly.

- `restic` **0.19.1**, the static binary from the upstream release, checked against a pinned sha256;
- `restic-zima` — backup jobs: run, daily timer, restore into a new folder;
- the **restic Backup** tile on the dashboard (`restic-web`).

## The tile

**New backup job:** a name, the repository — a folder (an attached disk, e.g. `/media/Backup/restic`) or a restic URL such as a [rest-server](https://github.com/restic/rest-server) (`rest:http://host:8000/name`) or `sftp:user@host:/path` — the password (empty: the tile generates one and shows it once), the folders to back up, optional exclude patterns and a daily time (empty: by hand only). Saving checks the repository with restic: an existing one must open with the password, a missing one is created when “create the repository” is ticked; a folder to back up that does not exist is refused.

Per job: **Back up now**, the last run (new and changed files, data added, warnings), the next run, **Snapshots** of the repository with **Restore to new folder** (into `/DATA/restic-restore/<job>-<snapshot>-<time>/`, never over the originals), **Show password**, **Delete job** (removes the timer; the job files with the password move to `jobs/.deleted/`, the repository is not touched).

**Nothing in this module forgets or prunes snapshots.** The repository grows with every run; thinning it out (`restic forget --prune` with a keep policy) deletes backups and stays a decision made by hand:

```sh
sudo restic-zima restic <job> forget --keep-daily 7 --keep-weekly 5 --keep-monthly 12 --dry-run
```

**A missing folder is a warning.** restic skips a path that does not exist and still exits 0 (measured with 0.19.1) — an unplugged disk would go unnoticed. `restic-zima run` counts such paths, exits 3 and the tile shows the run as a warning with the first missing folder.

## Files

```
/DATA/AppData/restic/jobs/<job>/
  repository   password   paths   excludes   schedule      what the tile wrote (0600)
  last.json    last.log                                    result of the last run
  restore.json restore.log                                 result of the last restore
/DATA/AppData/restic/cache/                                restic's cache (on /DATA: ZimaOS' /root is not where it belongs)
/etc/systemd/system/restic-zima-<job>.service              the job (oneshot)
/etc/systemd/system/restic-zima-<job>.timer                its daily time, Persistent=true
```

The units live in `/etc`, which survives reboots on ZimaOS; `After=systemd-sysext.service` makes a run that was missed while the host was off wait until `/usr/bin` from the module is merged (measured on 1.8.0: systemd-sysext.service active ~9 s after boot, before timers.target).

## Command line

```sh
sudo restic-zima list
sudo restic-zima run <job>                       # what the timer runs
sudo restic-zima restic <job> snapshots          # restic with the job's repository and password
sudo restic-zima restic <job> check
sudo restic-zima restore <job> <snapshot-id> /DATA/restic-restore/by-hand
```

## Install

```sh
sudo zpkg install /path/to/restic.raw
```

zpkg starts `restic-web`; the tile appears on the dashboard. Uninstalling the module leaves the jobs in `/DATA/AppData/restic` and their units in `/etc/systemd/system`; a timer left behind fails until the module is back — delete the jobs in the tile first if they should stop.

## Build

```sh
sh build.sh            # amd64; sh build.sh arm64 for arm64 (arm64 untested on a host)
```

Needs Go, `curl`, `bzip2`, `squashfs-tools` and [lintux-modkit](https://github.com/chicohaager/lintux-modkit) next to this repository (or `LINTUX_MODKIT=`). The backend tests run the real restic and `restic-zima` against a repository in a temp directory.

On a host: `sudo sh tools/host-test.sh` (command line and timer, as root), then from the build machine
`python3 tools/tile-test.py --url http://<host> --user <name> --password-file <file>` (the tile through the
gateway with a real login); `sudo sh tools/host-test.sh cleanup` removes what both created.

## License

MIT for this repository. restic is BSD-2-Clause (`/usr/share/doc/restic/LICENSE`).
