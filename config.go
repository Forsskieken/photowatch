package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Defaults. They live here and not scattered through the env example, so that
// there is one place where you can see what happens when a key is missing.
const (
	defaultSnapshotPrefix = "photowatch"
	// 1, because every deletion from a photo archive is news. Raising it can
	// make sense when Immich regularly throws away and rewrites XMP sidecars —
	// those are the notifications that say nothing; the payload counts media
	// files and sidecar files separately so you can judge that. This default and
	// /etc/photowatch/photowatch.env must name the same value.
	defaultThreshold = 1
	defaultKeepDays  = 14
	defaultReportDir = "/var/log/photowatch"
	defaultStateDir  = "/var/lib/photowatch"
	// Thumbnails: how many, how large, and above which total size we stop. 24 is
	// not a cross-section of 400 files but it is enough to see *what kind* of
	// material is gone; 320 pixels fits a phone screen and a file manager.
	defaultThumbnailMax   = 24
	defaultThumbnailPx    = 320
	defaultThumbnailMaxMB = 512
	// The text reports are the only long-term record of what ever disappeared,
	// and they are a few kilobytes each. A year is cheap.
	defaultKeepDaysReport = 365
	// Absolute path, so that PATH does not matter. On Debian and Proxmox zfs
	// lives in /usr/sbin; check with `command -v zfs`. If it differs, set
	// ZFS_PATH in the env file.
	defaultZfsPath = "/usr/sbin/zfs"
)

// Limits on the numeric configuration. Above the upper bound it is no longer a
// setting but a typo: a threshold of 100000 is always silent, and 3650 days of
// retention fills the pool.
const (
	maxThreshold = 1000000
	minKeepDays  = 2
	maxKeepDays  = 365

	minThumbnailMax   = 1
	maxThumbnailMax   = 200
	minThumbnailPx    = 64
	maxThumbnailPx    = 1024
	minThumbnailMaxMB = 1
	maxThumbnailMaxMB = 100000

	minKeepDaysReport = 14
	maxKeepDaysReport = 3650
)

// ZFS only allows these characters in a dataset name. We check it ourselves
// before handing it to the zfs command: the name comes from a file, and
// anything from outside is suspect, even when that file is mode 600.
var datasetRegexp = regexp.MustCompile(`^[A-Za-z0-9_.:/-]+$`)

// The snapshot prefix ends up after the @ and may therefore not contain a /.
var snapshotPrefixRegexp = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// Config is the complete setup of one run. Everything comes from the
// environment, which systemd fills from /etc/photowatch/photowatch.env (mode
// 600).
type Config struct {
	Dataset        string   // e.g. tank/photos
	PathPrefix     string   // empty = no filter; otherwise only paths below this
	SnapshotPrefix string   // name part before the date, e.g. "photowatch"
	Threshold      int      // from this many deleted files there is an alert
	KeepDays       int      // how long our own snapshots stay
	WebhookURL     *url.URL // full HA webhook URL, including the secret id
	HAIP           string   // emergency brake: connect to this IP, verify against the host name
	ReportDir      string
	StateDir       string
	ZfsPath        string

	// ThumbnailDir is empty when no thumbnails are made. If there is a path, it
	// must lie outside the mountpoint of the watched dataset — otherwise `zfs
	// diff` would see the thumbnails as added files tomorrow and, far worse,
	// Immich would import them as photos during its next scan of the external
	// library.
	ThumbnailDir   string
	ThumbnailMax   int
	ThumbnailPx    int
	ThumbnailMaxMB int
	KeepDaysReport int
}

// LoadConfig takes the values from the environment without validating them.
// Splitting reading from validating is needed for `-status`: that must also
// work when the rest of the config has not been filled in yet.
func LoadConfig() *Config {
	c := &Config{
		Dataset:        strings.TrimSpace(os.Getenv("DATASET")),
		PathPrefix:     strings.TrimSpace(os.Getenv("PATH_PREFIX")),
		SnapshotPrefix: defaultValue("SNAPSHOT_PREFIX", defaultSnapshotPrefix),
		HAIP:           strings.TrimSpace(os.Getenv("HA_IP")),
		ReportDir:      defaultValue("REPORT_DIR", defaultReportDir),
		StateDir:       defaultValue("STATE_DIR", defaultStateDir),
		ZfsPath:        defaultValue("ZFS_PATH", defaultZfsPath),
		// THRESHOLD and KEEP_DAYS are only read from the environment in
		// Validate: for a number, reading and validating coincide, and then you
		// do not want those two in two places.
		Threshold: defaultThreshold,
		KeepDays:  defaultKeepDays,

		ThumbnailDir:   strings.TrimSpace(os.Getenv("THUMBNAIL_DIR")),
		ThumbnailMax:   defaultThumbnailMax,
		ThumbnailPx:    defaultThumbnailPx,
		ThumbnailMaxMB: defaultThumbnailMaxMB,
		KeepDaysReport: defaultKeepDaysReport,
	}
	return c
}

func defaultValue(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Validate checks the configuration. Every error is permanent: retrying does
// not help against a wrongly filled in file, so main exits on this with code 2.
func (c *Config) Validate() error {
	var problems []string

	if c.Dataset == "" {
		problems = append(problems, "DATASET is empty; fill in the ZFS dataset, for example tank/photos")
	} else if !datasetRegexp.MatchString(c.Dataset) {
		problems = append(problems, fmt.Sprintf("DATASET %q contains characters that do not occur in a ZFS name", c.Dataset))
	} else if strings.HasPrefix(c.Dataset, "/") || strings.HasPrefix(c.Dataset, "-") {
		// A name starting with - would be read as a flag by zfs; a name starting
		// with / is a mountpoint, not a dataset.
		problems = append(problems, fmt.Sprintf("DATASET %q may not start with / or -", c.Dataset))
	}

	if !snapshotPrefixRegexp.MatchString(c.SnapshotPrefix) || strings.HasPrefix(c.SnapshotPrefix, "-") {
		problems = append(problems, fmt.Sprintf("SNAPSHOT_PREFIX %q may only contain letters, digits and _ . : - and may not start with -", c.SnapshotPrefix))
	}

	if c.PathPrefix != "" {
		if !strings.HasPrefix(c.PathPrefix, "/") {
			problems = append(problems, fmt.Sprintf("PATH_PREFIX %q must be an absolute path as zfs diff prints it, for example /mnt/tank/photos", c.PathPrefix))
		} else {
			// Without cleaning, /mnt/tank/photos/ would not match the path
			// /mnt/tank/photos/x.jpg, because we compare on prefix + "/".
			c.PathPrefix = filepath.Clean(c.PathPrefix)
		}
	}

	if err := checkInt("THRESHOLD", os.Getenv("THRESHOLD"), 1, maxThreshold, &c.Threshold); err != nil {
		problems = append(problems, err.Error())
	}
	if err := checkInt("KEEP_DAYS", os.Getenv("KEEP_DAYS"), minKeepDays, maxKeepDays, &c.KeepDays); err != nil {
		problems = append(problems, err.Error())
	}
	if err := checkInt("KEEP_DAYS_REPORT", os.Getenv("KEEP_DAYS_REPORT"), minKeepDaysReport, maxKeepDaysReport, &c.KeepDaysReport); err != nil {
		problems = append(problems, err.Error())
	}
	if err := checkInt("THUMBNAIL_MAX", os.Getenv("THUMBNAIL_MAX"), minThumbnailMax, maxThumbnailMax, &c.ThumbnailMax); err != nil {
		problems = append(problems, err.Error())
	}
	if err := checkInt("THUMBNAIL_PX", os.Getenv("THUMBNAIL_PX"), minThumbnailPx, maxThumbnailPx, &c.ThumbnailPx); err != nil {
		problems = append(problems, err.Error())
	}
	if err := checkInt("THUMBNAIL_MAX_MB", os.Getenv("THUMBNAIL_MAX_MB"), minThumbnailMaxMB, maxThumbnailMaxMB, &c.ThumbnailMaxMB); err != nil {
		problems = append(problems, err.Error())
	}
	if err := c.checkThumbnailDir(); err != nil {
		problems = append(problems, err.Error())
	}

	if err := c.checkWebhook(); err != nil {
		problems = append(problems, err.Error())
	}

	if c.HAIP != "" && net.ParseIP(c.HAIP) == nil {
		problems = append(problems, fmt.Sprintf("HA_IP %q is not a valid IP address; leave it empty when DNS works", c.HAIP))
	}

	for key, path := range map[string]string{"REPORT_DIR": c.ReportDir, "STATE_DIR": c.StateDir, "ZFS_PATH": c.ZfsPath} {
		if !filepath.IsAbs(path) {
			problems = append(problems, fmt.Sprintf("%s %q must be an absolute path", key, path))
		}
	}

	if len(problems) > 0 {
		return errors.New("configuration error: " + strings.Join(problems, "; "))
	}
	return nil
}

// checkThumbnailDir checks everything we can already know without the running
// ZFS pool. The most important check — does the directory lie inside the
// mountpoint of the watched dataset? — cannot happen here: the mountpoint only
// arrives during the run from `zfs get`. That one lives in main.go, like
// checkPathPrefix.
func (c *Config) checkThumbnailDir() error {
	if c.ThumbnailDir == "" {
		return nil
	}
	if !filepath.IsAbs(c.ThumbnailDir) {
		return fmt.Errorf("THUMBNAIL_DIR %q must be an absolute path, or stay empty when you do not want thumbnails", c.ThumbnailDir)
	}
	c.ThumbnailDir = filepath.Clean(c.ThumbnailDir)
	for _, forbidden := range forbiddenCleanupDirs {
		if c.ThumbnailDir == forbidden {
			return fmt.Errorf("THUMBNAIL_DIR %q is a system directory; pick a directory of your own, because photowatch cleans up inside it", c.ThumbnailDir)
		}
	}
	// Equal to REPORT_DIR or STATE_DIR would set two cleanup policies loose on
	// the same directory, with different retention periods.
	if c.ThumbnailDir == filepath.Clean(c.ReportDir) {
		return errors.New("THUMBNAIL_DIR equals REPORT_DIR; the thumbnails belong on the share, the reports and restore scripts do not")
	}
	if c.ThumbnailDir == filepath.Clean(c.StateDir) {
		return errors.New("THUMBNAIL_DIR equals STATE_DIR; that is where status.json lives")
	}
	return nil
}

func (c *Config) checkWebhook() error {
	raw := strings.TrimSpace(os.Getenv("WEBHOOK_URL"))
	if raw == "" {
		return errors.New("WEBHOOK_URL is empty; fill in https://home.example.net:8123/api/webhook/<id>")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// The URL holds the secret id, so it may not appear in the error.
		return errors.New("WEBHOOK_URL is not a valid URL (value not shown: it holds the webhook id)")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("WEBHOOK_URL uses scheme %q; only https is allowed, otherwise the webhook id travels the network unencrypted", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("WEBHOOK_URL has no host name")
	}
	if strings.Trim(u.Path, "/") == "" {
		return errors.New("WEBHOOK_URL is missing the path /api/webhook/<id>")
	}
	// A trailing / does not belong there: HA's endpoint is /api/webhook/<id>
	// without a slash. It is always a typo and the intent is unambiguous, so we
	// trim it instead of refusing the run. More importantly: with that slash the
	// last path segment is empty, and then masking that cuts at the last / would
	// put the webhook id in the log in full. MaskURL handles that itself by now;
	// this is the second safeguard.
	if strings.HasSuffix(u.Path, "/") {
		u.Path = strings.TrimRight(u.Path, "/")
		// RawPath is the undecoded form of the old path; after changing Path it
		// is no longer valid and String() has to escape by itself.
		u.RawPath = ""
	}
	if u.RawQuery != "" || u.Fragment != "" {
		// A webhook URL with a query or a fragment is not a webhook URL. We
		// refuse it instead of stripping it silently, because it points at a
		// badly pasted value and then you want to know.
		return errors.New("WEBHOOK_URL contains a ? or # (value not shown: it holds the webhook id); expected https://host:8123/api/webhook/<id>")
	}
	c.WebhookURL = u
	return nil
}

// checkInt reads an optional numeric key. When it is missing, the default the
// caller already put in target stays.
func checkInt(key, raw string, min, max int, target *int) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a whole number", key, raw)
	}
	if n < min || n > max {
		return fmt.Errorf("%s is %d; that must be between %d and %d", key, n, min, max)
	}
	*target = n
	return nil
}

// WithinPathPrefix reports whether a path from the diff counts. Without a
// prefix everything counts.
func (c *Config) WithinPathPrefix(path string) bool {
	return withinPathPrefix(c.PathPrefix, path)
}

func withinPathPrefix(prefix, path string) bool {
	if prefix == "" {
		return true
	}
	// Note the "/": without that check /mnt/tank/photobook would also fall
	// within /mnt/tank/photo.
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
