# Installing photowatch

Run as root on the host that holds the ZFS pool. Replace `tank/photos` and `family` with your own.

## 1. Find the dataset

```bash
zfs list -o name,mountpoint,used && command -v zfs
zfs snapshot tank/photos@photowatch-test
touch /mnt/tank/photos/probe.txt && rm /mnt/tank/photos/probe.txt
zfs diff -H -F tank/photos@photowatch-test tank/photos
zfs destroy tank/photos@photowatch-test
```

Expect tab-separated lines like `-⇥F⇥/mnt/tank/photos/probe.txt`. Are the photos a
directory inside a larger dataset? Set `DATASET` to that dataset and `PATH_PREFIX` to
the path exactly as `zfs diff` prints it.

## 2. Create the thumbnail dataset

Optional; skip it by leaving `THUMBNAIL_DIR` empty. It must lie **outside** the watched
dataset, otherwise Immich imports the thumbnails as photos. Share it read-only to see
them without a shell.

```bash
zfs create -o mountpoint=/mnt/tank/photowatch -o quota=2G -o atime=off tank/photowatch
groupadd family && usermod -aG family alex
chown root:family /mnt/tank/photowatch && chmod 750 /mnt/tank/photowatch
zfs get snapdir tank/photos    # must be 'hidden'; visible lets Immich import .zfs
```

## 3. Build and install

```bash
git clone https://github.com/Forsskieken/photowatch && cd photowatch
go vet ./... && go test ./... && CGO_ENABLED=0 go build -trimpath -o photowatch ./cmd/photowatch
install -o root -g root -m 755 photowatch /usr/local/bin/photowatch
install -d -o root -g root   -m 750 /etc/photowatch /var/lib/photowatch
install -d -o root -g family -m 750 /var/log/photowatch
install -o root -g root -m 600 etc/photowatch.env.example /etc/photowatch/photowatch.env
install -o root -g root -m 644 systemd/photowatch.service systemd/photowatch.timer \
    'systemd/photowatch-failure@.service' /etc/systemd/system/
systemctl daemon-reload
stat -c '%U %G %a' /etc/photowatch/photowatch.env   # must be: root root 600
```

`/var/log/photowatch` holds the deleted paths and the restore script: give it a small
group, or `root`.

## 4. Configure

```bash
openssl rand -hex 32                       # the webhook id
$EDITOR /etc/photowatch/photowatch.env     # DATASET, WEBHOOK_URL, THUMBNAIL_DIR
```

Changing `THUMBNAIL_DIR` means changing `ReadWritePaths=` in the service unit too: the
one value that appears twice.

## 5. Home Assistant

1. `secrets.yaml`: `photowatch_webhook: <the same id>`
2. `template.yaml`: the block from [`homeassistant/template-photowatch.yaml`](../homeassistant/template-photowatch.yaml)
3. `automations.yaml`: the four automations from [`homeassistant/automations-photowatch.yaml`](../homeassistant/automations-photowatch.yaml), with your own notify services
4. Developer tools → YAML → check configuration, then **restart HA fully**; reloading templates does not register the webhook

```bash
URL=$(sed -n 's/^WEBHOOK_URL=//p' /etc/photowatch/photowatch.env)
curl -sS -o /dev/null -w '%{http_code}\n' "$URL"
```

`405` = registered and reachable. `200` = not registered, or refused by `local_only`;
restart HA before touching `local_only`.

## 6. First run

```bash
systemd-run --pipe --wait --collect --unit=photowatch-dry \
  -p EnvironmentFile=/etc/photowatch/photowatch.env \
  /usr/local/bin/photowatch -dry-run -debug
systemctl start photowatch.service && journalctl -u photowatch -n 50 --no-pager
systemctl enable --now photowatch.timer
```

A dry run writes into the `dry-run/` subdirectories only and sends nothing. Use
`systemd-run -p EnvironmentFile=` and not `set -a; . file`: the shell expands `$` and
strips quotes, systemd does neither.

## 7. Prove it end to end

```bash
mkdir -p '/mnt/tank/photos/probe'
cp <a real photo> '/mnt/tank/photos/probe/sunset at the café.jpg'
systemctl start photowatch.service     # now it is in the snapshot
rm -r '/mnt/tank/photos/probe' && systemctl start photowatch.service
bash /var/log/photowatch/restore-$(date +%F).sh --apply
```

You know it worked when a notification arrives, `deleted-<today>.txt` shows the `é`
readably, the file is back after `--apply`, and the journal holds no full webhook id.
