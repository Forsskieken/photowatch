# Photos disappeared. Now what?

The notification says something is *gone*, not that something is broken.

## Two nets, in this order

| Net | How long | Where |
|---|---|---|
| Immich trash | 30 days | in the app, *before* this alert |
| ZFS snapshot | `KEEP_DAYS`, default 14 | on the storage host, root needed |

By the time this alert arrives the Immich trash is no longer a way out: a photo deleted
in the app only leaves the disk when that trash empties, and only then does photowatch
see it. The notification names the last date on which restoring is still possible.

## Look at what is gone

The notification holds the path to a day directory on the share, for example
`/mnt/tank/photowatch/2026-08-31`, with thumbnails and a `report.txt` holding the full
list. Not there yet? Wait a minute: the images are made after the message is sent, on
purpose. If the directory stays away, no thumbnail could be made at all (video, raw,
unreadable); the list is in the report either way.

## Put it back

```bash
bash /var/log/photowatch/restore-2026-08-31.sh            # shows, changes nothing
bash /var/log/photowatch/restore-2026-08-31.sh --apply    # really restores
```

Run it as root on the storage host, and take the path from your notification: if the
watch ran twice that day, `restore-2026-08-31-2.sh` covers entirely different files.

The script can only **create** files: it never overwrites, never deletes, leaves existing
directories alone, and refuses without root, without the snapshot, without `rsync` or
without its list. Running it twice is harmless. A directory that had disappeared too
comes back owned by `root`; check it with `ls -ld`.

**Never move `restore-*.sh` to the share.** It runs as root and belongs in a directory
only root can write to.

To restore part of it, cut lines from the NUL-separated list next to it and point
`LIST=` in a copy of the script at the result:

```bash
tr '\0' '\n' < /var/log/photowatch/restore-2026-08-31.list \
  | grep holiday | tr '\n' '\0' > /root/part.list
```

## No script?

The report says why and gives an `rsync` line with a real path filled in. A path with a
`!` in front could not be decoded and is shown in the raw form of `zfs diff` (`\0040` is
a space, `\0134` a backslash); convert it by hand and check with `ls` before copying.
