# Operating photowatch

## Three layers of monitoring

| Layer | Where | What it tells you |
|---|---|---|
| `status.json` | `/var/lib/photowatch/status.json`, or `photowatch -status` | the last run, in full |
| `sensor.photowatch_last_run` | Home Assistant | moves every day |
| "Photowatch - task is silent" | Home Assistant | complains when that sensor is 30 hours old |

`reported: true` in `status.json` only means Home Assistant accepted the message. An
unknown webhook id and a request refused by `local_only` both also answer 200; that is
what the dead man's switch above is for.

## Commands

```bash
journalctl -u photowatch -f                          # follow live
journalctl -u photowatch -p err --since '7 days ago'
systemctl list-timers photowatch.timer               # when does it run again
photowatch -status                                   # or: cat status.json
ls -l /var/log/photowatch/ /mnt/tank/photowatch/     # reports and thumbnails
zfs list -o name,used,quota tank/photowatch          # at its quota?
```

Two INFO lines per run tell the whole story: `photowatch done` (both snapshot names, all
counters, the duration up to the alert) and `aftercare done` (thumbnails, files cleaned
up, side issues). Is the second one missing, the process died in between.

## When it goes wrong

| Situation | What the program does |
|---|---|
| Home Assistant away, or 5xx | three attempts with 5 and 15 seconds between them, then ERROR and exit 1 |
| Wrong webhook id | nothing visible: HA answers 200 to an unknown id on purpose. After 30 hours "task is silent" fires |
| 4xx from HA or a proxy | stop at once; check the path, `allowed_methods`, and any proxy |
| Configuration error | stop before anything happens, exit 2. The OnFailure unit reports it with the error in the text |
| Reference snapshot gone | a new reference is set, but the run reports `status: error` |
| `zfs diff` output unparsable | `status: error` instead of a reassuring zero. Individual unparsed lines only get counted |
| `PATH_PREFIX` matches nothing | checked at startup: `status: error`. A day where everything simply fell outside it is not an error |
| Failure during the run | one message, not two: the run reports itself and the OnFailure unit sees the same invocation id and stays silent |
| Twice on one day | the second run writes under `-2` and leaves the first set untouched |
| Host was off at 08:15 | `Persistent=true` catches up as soon as it is on |
| Cleanup backlog | at most 5 snapshots and 20 files per run, with a WARN; the rest follows |
| Thumbnails fail | WARN plus a line in the notification; the alert, the report and the restore script still arrive |
| `THUMBNAIL_DIR` inside the watched dataset | `status: error` and no thumbnail at all; Immich would import them as photos |
| Mountpoint not available | no thumbnails, no restore script, and `THUMBNAIL_DIR` is not cleaned that run |
| Process dies after the notification | alert, restore script and `status.json` are already there; the next run does the cleanup |
| `artifacts_mb` keeps growing | the "disk space" automation reports above 800 MB: the cleanup is stuck |
| Hard abort while writing | a `.photowatch-<digits>` may stay behind; the next write removes it after a day |
