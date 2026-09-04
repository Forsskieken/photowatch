package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setEnv puts down a complete, valid environment; every test changes one thing
// in it. t.Setenv restores everything afterwards.
func setEnv(t *testing.T, extra map[string]string) *Config {
	t.Helper()
	base := map[string]string{
		"DATASET":         "tank/photos",
		"PATH_PREFIX":     "",
		"SNAPSHOT_PREFIX": "",
		"THRESHOLD":       "",
		"KEEP_DAYS":       "",
		"WEBHOOK_URL":     "https://home.example.net:8123/api/webhook/" + testWebhookID,
		"HA_IP":           "",
		"REPORT_DIR":      "",
		"STATE_DIR":       "",
		"ZFS_PATH":        "",
	}
	for k, v := range extra {
		base[k] = v
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
	return LoadConfig()
}

func TestConfigDefaults(t *testing.T) {
	c := setEnv(t, nil)
	if err := c.Validate(); err != nil {
		t.Fatalf("a valid configuration was rejected: %v", err)
	}
	// 1 and not 20: every deletion from the album is news. This value and
	// etc/photowatch.env.example must say the same thing.
	if c.Threshold != 1 {
		t.Errorf("threshold = %d, want 1", c.Threshold)
	}
	if c.KeepDays != 14 {
		t.Errorf("keep days = %d, want 14", c.KeepDays)
	}
	if c.SnapshotPrefix != "photowatch" {
		t.Errorf("snapshot prefix = %q, want photowatch", c.SnapshotPrefix)
	}
	if c.ReportDir != defaultReportDir || c.StateDir != defaultStateDir {
		t.Errorf("default directories are wrong: %s, %s", c.ReportDir, c.StateDir)
	}
}

func TestConfigRejectsWhatIsWrong(t *testing.T) {
	cases := map[string]map[string]string{
		"empty dataset":            {"DATASET": ""},
		"dataset with space":       {"DATASET": "tank/my photos"},
		"dataset with semicolon":   {"DATASET": "tank/photos;rm -rf /"},
		"dataset as a flag":        {"DATASET": "-tank"},
		"http instead of https":    {"WEBHOOK_URL": "http://home.example.net:8123/api/webhook/x"},
		"webhook without path":     {"WEBHOOK_URL": "https://home.example.net:8123/"},
		"empty webhook":            {"WEBHOOK_URL": ""},
		"threshold zero":           {"THRESHOLD": "0"},
		"threshold not a number":   {"THRESHOLD": "many"},
		"keep days too short":      {"KEEP_DAYS": "1"},
		"keep days too long":       {"KEEP_DAYS": "4000"},
		"ha_ip not an address":     {"HA_IP": "home.example.net"},
		"relative report dir":      {"REPORT_DIR": "logs"},
		"snapshot prefix with /":   {"SNAPSHOT_PREFIX": "photo/watch"},
		"path prefix not absolute": {"PATH_PREFIX": "photos"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			c := setEnv(t, extra)
			err := c.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			// An error message may not pass on the webhook id.
			if strings.Contains(err.Error(), testWebhookID) {
				t.Errorf("the webhook id appears in the error: %v", err)
			}
		})
	}
}

func TestConfigCleansPathPrefix(t *testing.T) {
	c := setEnv(t, map[string]string{"PATH_PREFIX": "/mnt/tank/photos/"})
	if err := c.Validate(); err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if c.PathPrefix != "/mnt/tank/photos" {
		t.Errorf("path prefix = %q, want /mnt/tank/photos without a trailing slash", c.PathPrefix)
	}
	if !c.WithinPathPrefix("/mnt/tank/photos/2019/x.jpg") {
		t.Error("a path inside the prefix fell outside it")
	}
	if c.WithinPathPrefix("/mnt/tank/photobook/x.jpg") {
		t.Error("a path outside the prefix counted")
	}
}

// A trailing / in WEBHOOK_URL broke the masking: the last path segment is empty
// then and the webhook id stayed in the log in full. checkWebhook trims it;
// MaskURL has a safeguard of its own for it.
func TestConfigTrimsTrailingSlashFromWebhook(t *testing.T) {
	c := setEnv(t, map[string]string{
		"WEBHOOK_URL": "https://home.example.net:8123/api/webhook/" + testWebhookID + "/",
	})
	if err := c.Validate(); err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if c.WebhookURL.Path != "/api/webhook/"+testWebhookID {
		t.Errorf("path = %q, want the path without a trailing slash", c.WebhookURL.Path)
	}
	if c.WebhookURL.String() != "https://home.example.net:8123/api/webhook/"+testWebhookID {
		t.Errorf("URL = %q; after trimming String() must still be right", c.WebhookURL.String())
	}
	if got := MaskURL(c.WebhookURL); strings.Contains(got, testWebhookID) {
		t.Errorf("the webhook id is still in the masked URL: %s", got)
	}
}

func TestConfigRejectsWebhookWithQueryOrFragment(t *testing.T) {
	for name, value := range map[string]string{
		"query":    "https://home.example.net:8123/api/webhook/" + testWebhookID + "?x=1",
		"fragment": "https://home.example.net:8123/api/webhook/" + testWebhookID + "#top",
	} {
		t.Run(name, func(t *testing.T) {
			c := setEnv(t, map[string]string{"WEBHOOK_URL": value})
			err := c.Validate()
			if err == nil {
				t.Fatal("was accepted")
			}
			if strings.Contains(err.Error(), testWebhookID) {
				t.Errorf("the webhook id appears in the error: %v", err)
			}
		})
	}
}

func TestCleanTextStripsControlChars(t *testing.T) {
	got := cleanText("unit photowatch.service\nfailed\r\nERROR: fake", 200)
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("there are still newlines in it: %q", got)
	}
	long := strings.Repeat("x", 500)
	if n := len([]rune(cleanText(long, 200))); n > 201 {
		t.Errorf("text is %d characters, want at most 201", n)
	}
}

// The env example once undid the decision about THRESHOLD: whoever followed the
// install instructions after a reinstall silently got 20 back, while
// docs/RECOVERY.md promises that one vanished file is already enough for a
// notification.
//
// This test therefore compares every numeric key in the example file with the
// default in the code. If one of them ever deviates on purpose, that should be
// an explicit conversation and not something you notice a year later.
func TestEnvExampleMatchesTheDefaults(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("etc", "photowatch.env.example"))
	if err != nil {
		t.Fatalf("example file not readable: %v", err)
	}
	want := map[string]int{
		"THRESHOLD":        defaultThreshold,
		"KEEP_DAYS":        defaultKeepDays,
		"THUMBNAIL_MAX":    defaultThumbnailMax,
		"THUMBNAIL_PX":     defaultThumbnailPx,
		"THUMBNAIL_MAX_MB": defaultThumbnailMaxMB,
		"KEEP_DAYS_REPORT": defaultKeepDaysReport,
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		expected, wanted := want[key]
		if !wanted {
			continue
		}
		seen[key] = true
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Errorf("%s=%q is not a number: %v", key, value, err)
			continue
		}
		if n != expected {
			t.Errorf("%s is %d in etc/photowatch.env.example and %d in config.go; "+
				"whoever follows docs/INSTALL.md then silently gets the other value",
				key, n, expected)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("%s is missing from etc/photowatch.env.example", key)
		}
	}
}
