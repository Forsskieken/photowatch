# photowatch

Reports deleted photos through Home Assistant and lets you get them back from a
ZFS snapshot.

## What it does

- **Daily scan.** Diffs the dataset against yesterday's snapshot, takes today's
  snapshot and prunes snapshots older than `KEEP_DAYS`.
- **Alert.** From `THRESHOLD` deleted files a notification goes to your phone,
  with the folders, the recovery deadline and the path to the restore script.
- **Heartbeat.** It reports every day, also when nothing is gone. If that stops,
  Home Assistant raises the alarm after 30 hours.
- **Paperwork.** A report with the readable paths, a ready-made restore script
  and thumbnails of the vanished photos on a share.
- **Manual restore.** No button in Home Assistant. The script shows what it
  would do until you pass `--apply`, and can only create files.

## Requirements

| | |
|---|---|
| Host | Linux with OpenZFS, systemd, root access to `/dev/zfs`, `rsync` |
| Build | Go 1.22 or newer, standard library only, no `go.sum` |
| Home Assistant | webhook trigger and the mobile app integration |
| Thumbnails | a second ZFS dataset outside the watched one, on a share (optional) |

## Install

Ten minutes, in [docs/INSTALL.md](docs/INSTALL.md): build, install, configure,
add three pieces of YAML to Home Assistant, enable the timer.

## Configure

Everything comes from the environment; systemd reads
`/etc/photowatch/photowatch.env` (root:root, mode 600).

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DATASET` | yes | — | ZFS dataset to snapshot, e.g. `tank/photos` |
| `WEBHOOK_URL` | yes | — | `https://host:8123/api/webhook/<id>`, https only. **The secret** |
| `PATH_PREFIX` | no | empty | Count only paths below this, as `zfs diff` prints them |
| `SNAPSHOT_PREFIX` | no | `photowatch` | Name part before the date |
| `THRESHOLD` | no | `1` | Alert from this many deleted files |
| `KEEP_DAYS` | no | `14` | How long own snapshots stay (2–365) |
| `HA_IP` | no | empty | Connect to this IP when DNS fails; certificate still checked against the host name |
| `REPORT_DIR` | no | `/var/log/photowatch` | Reports, restore scripts and lists |
| `STATE_DIR` | no | `/var/lib/photowatch` | `status.json` |
| `ZFS_PATH` | no | `/usr/sbin/zfs` | Absolute path to `zfs` |
| `THUMBNAIL_DIR` | no | empty | Thumbnails, one subdirectory per day. **Must lie outside the watched dataset** |
| `THUMBNAIL_MAX` | no | `24` | Thumbnails per run (1–200) |
| `THUMBNAIL_PX` | no | `320` | Long side in pixels (64–1024) |
| `THUMBNAIL_MAX_MB` | no | `512` | Stop making thumbnails above this total size |
| `KEEP_DAYS_REPORT` | no | `365` | How long text reports stay (14–3650) |

Generate the webhook id with `openssl rand -hex 32` and put the same id in Home
Assistant's secrets file.

## Verify

```bash
photowatch -dry-run -debug     # compute everything, write only into dry-run/
systemctl start photowatch.service && journalctl -u photowatch -n 50 --no-pager
photowatch -status
```

The first run finds no previous snapshot: it only sets the reference and says
so. The run of the next day measures for real.

## Restore and operate

A deleted file is back in five minutes as long as the snapshot exists:
[docs/RECOVERY.md](docs/RECOVERY.md). Logs, the status check and the failure
table: [docs/OPERATIONS.md](docs/OPERATIONS.md).

## Limitations

- No automatic restore and no button for it, on purpose. One dataset per instance.
- No thumbnails of video, HEIC or raw; they are in the report with the reason.
  A thumbnail can be sideways: the EXIF orientation is not read.
- No image in the push notification, only the path to the thumbnails.
- Snapshots protect against deletion, not against fire, theft or a broken pool.
- Runs as root, because ZFS needs `/dev/zfs`. The unit is locked down tightly.

## License

MIT — see [LICENSE](LICENSE).
