package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestState puts down a status.json the way a real run would.
func writeTestState(t *testing.T, dir string, sf StateFile) {
	t.Helper()
	data, err := json.Marshal(sf)
	if err != nil {
		t.Fatalf("state cannot be encoded: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), data, 0o640); err != nil {
		t.Fatalf("state cannot be written: %v", err)
	}
}

// One failure should produce one message. The run reports itself and exits with
// code 1; systemd then starts the OnFailure unit with the invocation id of that
// same run in the environment, and then it must stay silent.
func TestReportFailureOnlyStaysSilentWhenTheRunAlreadyReported(t *testing.T) {
	dir := t.TempDir()
	writeTestState(t, dir, StateFile{
		Written:    time.Now().Add(-30 * time.Second).Format(time.RFC3339),
		Reported:   true,
		Invocation: "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f",
		Payload:    Payload{Version: 1, Status: "error", Message: "zfs diff failed"},
	})

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// WEBHOOK_URL deliberately empty: if the code does reach sending, it
	// stumbles over the validation and we see that in the exit code.
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("MONITOR_INVOCATION_ID", "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f")
	cfg := &Config{StateDir: dir}

	if code := reportFailureOnly(context.Background(), cfg, log, "unit photowatch.service has failed"); code != exitOK {
		t.Errorf("exit code = %d, want %d: no second message should go out", code, exitOK)
	}
	if !strings.Contains(buf.String(), "no second message") {
		t.Errorf("the log does not explain why nothing was sent: %s", buf.String())
	}
}

func TestReportFailureOnlyReportsWhenTheRunCouldNot(t *testing.T) {
	const thisRun = "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f"
	cases := map[string]struct {
		sf      StateFile
		monitor string
	}{
		"reporting failed": {
			sf: StateFile{Written: time.Now().Format(time.RFC3339), Reported: false,
				Invocation: thisRun, Payload: Payload{Status: "error"}},
			monitor: thisRun,
		},
		"previous run was fine": {
			sf: StateFile{Written: time.Now().Format(time.RFC3339), Reported: true,
				Invocation: thisRun, Payload: Payload{Status: "ok"}},
			monitor: thisRun,
		},
		// The case the fifteen-minute window broke on: the previous run fell over
		// and reported itself, this run fell over within that same window too and
		// could no longer do anything. Different invocation, so report.
		"a different run than the one in status.json": {
			sf: StateFile{Written: time.Now().Add(-2 * time.Minute).Format(time.RFC3339), Reported: true,
				Invocation: thisRun, Payload: Payload{Status: "error"}},
			monitor: "0000000000000000000000000000ffff",
		},
		// Older systemd or started by hand: then it cannot be determined whether
		// it is about the same run, and we would rather report twice.
		"no invocation in status.json": {
			sf: StateFile{Written: time.Now().Format(time.RFC3339), Reported: true,
				Payload: Payload{Status: "error"}},
			monitor: thisRun,
		},
		"no invocation in the environment": {
			sf: StateFile{Written: time.Now().Format(time.RFC3339), Reported: true,
				Invocation: thisRun, Payload: Payload{Status: "error"}},
			monitor: "",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestState(t, dir, c.sf)
			t.Setenv("WEBHOOK_URL", "")
			t.Setenv("MONITOR_INVOCATION_ID", c.monitor)
			buf := &bytes.Buffer{}
			log := slog.New(slog.NewTextHandler(buf, nil))
			// Without WEBHOOK_URL it reaches the validation and stops there; that
			// proves it *wanted* to send. Going further would require a real
			// HTTPS server.
			if code := reportFailureOnly(context.Background(), &Config{StateDir: dir}, log, "unit has failed"); code != exitConfigError {
				t.Errorf("exit code = %d, want %d: this failure still has to be reported", code, exitConfigError)
			}
		})
	}
}

// Without status.json (never run, or /var/lib emptied) the failure reporter
// must simply try.
func TestReportFailureOnlyWithoutStateFile(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("MONITOR_INVOCATION_ID", "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f")
	buf := &bytes.Buffer{}
	code := reportFailureOnly(context.Background(), &Config{StateDir: t.TempDir()},
		slog.New(slog.NewTextHandler(buf, nil)), "unit has failed")
	if code != exitConfigError {
		t.Errorf("exit code = %d, want %d", code, exitConfigError)
	}
}

func TestSameRun(t *testing.T) {
	const id = "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f"
	reportedFailure := StateFile{Reported: true, Invocation: id, Payload: Payload{Status: "error"}}
	cases := map[string]struct {
		sf      StateFile
		monitor string
		want    bool
	}{
		"same run, already reported":  {reportedFailure, id, true},
		"different run":               {reportedFailure, "0000000000000000000000000000ffff", false},
		"no id in the environment":    {reportedFailure, "", false},
		"no id in status.json":        {StateFile{Reported: true, Payload: Payload{Status: "error"}}, id, false},
		"same run, reporting failed":  {StateFile{Invocation: id, Payload: Payload{Status: "error"}}, id, false},
		"same run, but not a failure": {StateFile{Reported: true, Invocation: id, Payload: Payload{Status: "ok"}}, id, false},
	}
	for name, c := range cases {
		if got := sameRun(c.sf, c.monitor); got != c.want {
			t.Errorf("%s: sameRun = %v, want %v", name, got, c.want)
		}
	}
}

// If the column layout of `zfs diff` changes, every line is unparsed and
// without this rule the run would report a tidy "deleted: 0".
func TestDiffUnusable(t *testing.T) {
	if !diffUnusable(&DiffResult{Unparsed: 1200}) {
		t.Error("output of which no line was understood must count as unusable")
	}
	// The setup DATASET=tank with PATH_PREFIX=/mnt/tank/photos: no photos added
	// or removed for 24 hours, but written elsewhere in the dataset. That is a
	// normal day and may not raise an alarm. Whether PATH_PREFIX still matches
	// anything is checked by checkPathPrefix at startup.
	if diffUnusable(&DiffResult{Skipped: 8000}) {
		t.Error("only lines outside the path prefix is a normal day with PATH_PREFIX, not a failure")
	}
	if diffUnusable(&DiffResult{Skipped: 8000, Added: 3}) {
		t.Error("skipped lines next to understood lines are normal: the dataset holds more than the photo directory")
	}
	if diffUnusable(&DiffResult{Unparsed: 2, Added: 5}) {
		t.Error("individual unparsed lines next to understood lines are not a failure")
	}
	if diffUnusable(&DiffResult{}) {
		t.Error("an empty diff (nothing happened) is not a failure")
	}
	if diffUnusable(&DiffResult{Unparsed: 3, Deleted: []DeletedFile{{Raw: "/mnt/tank/photos/x.jpg", Path: "/mnt/tank/photos/x.jpg", Decodable: true}}}) {
		t.Error("a deleted file next to unparsed lines is not a failure")
	}
}

func TestEarlierOrUnknown(t *testing.T) {
	if earlierOrUnknown("  ") != "unknown" {
		t.Error("an empty timestamp must be made readable")
	}
	if earlierOrUnknown("2026-08-29T08:15:00+02:00") != "2026-08-29T08:15:00+02:00" {
		t.Error("a valid timestamp must stay")
	}
}

// The check that replaces the false alarm of diffUnusable: if PATH_PREFIX does
// not match the dataset, the watch counts zero every day without ever measuring
// anything, and that must show at startup.
func TestCheckPathPrefix(t *testing.T) {
	mountpoint := t.TempDir()
	photos := filepath.Join(mountpoint, "Photos")
	if err := os.Mkdir(photos, 0o750); err != nil {
		t.Fatalf("test directory cannot be created: %v", err)
	}
	file := filepath.Join(mountpoint, "loose.txt")
	if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
		t.Fatalf("test file cannot be written: %v", err)
	}
	link := filepath.Join(mountpoint, "PhotoLink")
	if err := os.Symlink(photos, link); err != nil {
		t.Fatalf("symlink cannot be created: %v", err)
	}

	good := map[string]string{
		"prefix under the mountpoint": photos,
		"prefix is the mountpoint":    mountpoint,
		"empty prefix (the default)":  "",
		"trailing slash":              photos + "/",
	}
	for name, prefix := range good {
		if err := checkPathPrefix(mountpoint, prefix); err != nil {
			t.Errorf("%s: wrongly rejected: %v", name, err)
		}
	}

	bad := map[string]string{
		"outside the mountpoint":                 "/somewhere/else",
		"typo deeper in the path":                filepath.Join(mountpoint, "photos"),
		"points at a file":                       file,
		"symlink (zfs diff gives the real path)": link,
	}
	for name, prefix := range bad {
		if err := checkPathPrefix(mountpoint, prefix); err == nil {
			t.Errorf("%s: should have given an error", name)
		}
	}

	// A mountpoint of "/" is a valid exception: then every absolute path falls
	// inside it, while the ordinary prefix comparison would end up on "//" and
	// never match.
	if err := checkPathPrefix("/", "/tmp"); err != nil {
		t.Errorf("mountpoint / with an existing directory was rejected: %v", err)
	}
}

// Without a field that changes per run, a failure that repeats identically
// every day reports only once: Home Assistant then sees the same state *and*
// the same attributes and fires no state change.
func TestEveryNotificationHasItsOwnTimestamp(t *testing.T) {
	first := newPayload(time.Date(2026, 8, 30, 8, 15, 3, 0, time.Local))
	second := newPayload(time.Date(2026, 8, 31, 8, 17, 45, 0, time.Local))
	if first.Time == "" || second.Time == "" {
		t.Fatal("the timestamp is missing from the payload")
	}
	if first.Time == second.Time {
		t.Errorf("two runs got the same timestamp (%s); then the notification stays out", first.Time)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.Time); err != nil {
		t.Errorf("the timestamp is not RFC3339 and then HA cannot compute with it: %v", err)
	}
	// Two runs within the same second (started by hand) must differ too; that is
	// what the nanoseconds are for.
	base := time.Date(2026, 8, 30, 8, 15, 3, 0, time.Local)
	if newPayload(base).Time == newPayload(base.Add(time.Millisecond)).Time {
		t.Error("two runs within the same second got the same timestamp")
	}
	// The payload of the OnFailure path has no other field that differs per run;
	// the whole notification hangs on this stamp there.
	if newPayload(base).Examples == nil {
		t.Error("Examples should be an empty list, not nil")
	}
}

// fakeZfs puts down an executable script that pretends to be `zfs`. It is the
// minimum a whole run can be walked through with: a dataset, one previous
// snapshot of yesterday, one deleted file, and snapshot/destroy that do
// nothing. That way the order of the run can be measured without a ZFS pool.
func fakeZfs(t *testing.T, dataset, mountpoint, deletedPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zfs")
	script := "#!/bin/bash\n" +
		"case \"$1\" in\n" +
		"  list)\n" +
		"    if [[ \"$*\" == *\"-t snapshot\"* ]]; then\n" +
		"      printf '" + dataset + "@photowatch-2026-08-30\\t" +
		fmt.Sprint(time.Now().Add(-24*time.Hour).Unix()) + "\\n'\n" +
		"    else\n" +
		"      echo '" + dataset + "'\n" +
		"    fi ;;\n" +
		"  get) echo '" + mountpoint + "' ;;\n" +
		// %s and not the value in the format string itself: bash printf would
		// otherwise interpret the octal escapes of zfs (\\0040 for a space).
		"  diff) printf -- '-\\tF\\t%s\\n' '" + deletedPath + "' ;;\n" +
		"  snapshot|destroy) exit 0 ;;\n" +
		"  *) echo \"fakeZfs does not know $1\" >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("fake zfs cannot be written: %v", err)
	}
	return path
}

// The risky work belongs after the notification. This test measures the order
// that enforces it: when run() is done — and therefore just before start()
// sends the message — the payload must be complete and the restore script must
// be on disk, while the report, the thumbnails and the cleanup have not
// happened yet. If the process falls over after that (OOM, panic in a decoder,
// systemctl stop), the alert is already out.
func TestOrderRiskyWorkAfterTheNotification(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	photo := putImage(t, source, "From the old box/PICT0033 (5).JPG", 640, 480, "jpeg")
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	stateDir := t.TempDir()

	// Twenty reports from 2020: fourteen stay (guardrail 6), the oldest six
	// should go this run — but only after the notification.
	makeDatedFiles(t, reportDir, "deleted-%s.txt", 20, time.Date(2020, 1, 20, 8, 0, 0, 0, time.Local))
	oldReport := filepath.Join(reportDir, "deleted-2020-01-01.txt")

	cfg := &Config{
		Dataset:        "tank/photos",
		SnapshotPrefix: "photowatch",
		Threshold:      1,
		KeepDays:       14,
		ReportDir:      reportDir,
		StateDir:       stateDir,
		ZfsPath:        fakeZfs(t, "tank/photos", source.Mountpoint, photo.Raw),
		ThumbnailDir:   thumbDir,
		ThumbnailMax:   24,
		ThumbnailPx:    320,
		ThumbnailMaxMB: 512,
		KeepDaysReport: 365,
	}

	log, _ := captureLog()
	p, plan, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if p.Deleted != 1 || !p.Alert {
		t.Fatalf("payload counts %d deleted, alert=%v; want 1 and true", p.Deleted, p.Alert)
	}

	// What has to be finished before the notification.
	if p.RestoreScript == "" {
		t.Error("the payload names no restore script")
	} else if _, err := os.Stat(p.RestoreScript); err != nil {
		t.Errorf("the restore script is not there before the notification: %v", err)
	}
	if p.MediaFiles != 1 || len(p.Folders) == 0 || p.RecoverUntil == "" {
		t.Errorf("the payload is not complete: media=%d folders=%v until=%q",
			p.MediaFiles, p.Folders, p.RecoverUntil)
	}
	dayDir := filepath.Join(thumbDir, time.Now().Format("2006-01-02"))
	if p.Thumbnails != dayDir || p.ThumbnailsPlanned != 1 {
		t.Errorf("thumbnails=%q planned=%d, want %q and 1", p.Thumbnails, p.ThumbnailsPlanned, dayDir)
	}

	// And what may not have happened yet.
	if p.Report == "" {
		t.Error("the payload names no report path")
	} else if _, err := os.Stat(p.Report); !os.IsNotExist(err) {
		t.Errorf("the report was already written before the notification (err=%v)", err)
	}
	if _, err := os.Stat(dayDir); !os.IsNotExist(err) {
		t.Errorf("the day directory with thumbnails already exists before the notification (err=%v)", err)
	}
	if _, err := os.Stat(oldReport); err != nil {
		t.Errorf("cleanup already happened before the notification: %v", err)
	}

	// Now the work that only comes up after sending.
	result := runAftercare(context.Background(), log, plan)
	if result.Thumbnails != 1 {
		t.Errorf("aftercare made %d thumbnails, want 1 (side issues: %q)", result.Thumbnails, result.SideIssues)
	}
	// Twenty old ones plus today's fresh report, fourteen stay.
	if result.CleanedUp != 7 {
		t.Errorf("aftercare cleaned up %d items, want 7 old reports", result.CleanedUp)
	}
	if _, err := os.Stat(oldReport); !os.IsNotExist(err) {
		t.Errorf("the old report was not cleaned up (err=%v)", err)
	}
	data, err := os.ReadFile(p.Report)
	if err != nil {
		t.Fatalf("the report was not written even after the aftercare: %v", err)
	}
	// The report refers with [001] to the thumbnail; that only works when it is
	// written after the thumbnails.
	if !strings.Contains(string(data), "[001]") {
		t.Errorf("the report holds no thumbnail number:\n%s", data)
	}
	items, err := os.ReadDir(dayDir)
	if err != nil {
		t.Fatalf("day directory not readable: %v", err)
	}
	names := []string{}
	for _, i := range items {
		names = append(names, i.Name())
	}
	if len(names) != 2 {
		t.Errorf("day directory holds %v, want one thumbnail and report.txt", names)
	}
}

// The side issues of the aftercare can no longer be in today's message. They
// travel via status.json with the *next* run, so that a failure that persists
// still reaches the phone.
func TestSideIssuesOfThePreviousRunComeBackInThePayload(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	photo := putImage(t, source, "From the old box/PICT0033 (5).JPG", 64, 48, "jpeg")
	stateDir := t.TempDir()
	writeTestState(t, stateDir, StateFile{
		Written:   time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
		Reported:  true,
		Payload:   Payload{Version: 1, Status: "ok"},
		Aftercare: &AftercareResult{SideIssues: "cleanup failed: /var/log/photowatch cannot be walked"},
	})

	cfg := &Config{
		Dataset:        "tank/photos",
		SnapshotPrefix: "photowatch",
		Threshold:      1,
		KeepDays:       14,
		ReportDir:      t.TempDir(),
		StateDir:       stateDir,
		ZfsPath:        fakeZfs(t, "tank/photos", source.Mountpoint, photo.Raw),
		ThumbnailMax:   24,
		ThumbnailPx:    320,
		ThumbnailMaxMB: 512,
		KeepDaysReport: 365,
	}
	log, _ := captureLog()
	p, _, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if !strings.Contains(p.SideIssues, "on the previous run") ||
		!strings.Contains(p.SideIssues, "cannot be walked") {
		t.Errorf("side_issues = %q; the side issues of the previous run are missing", p.SideIssues)
	}
}

// testConfig is the configuration the integration tests of run() use.
func testConfig(t *testing.T, source SnapshotSource, deletedPath, reportDir, thumbDir, stateDir string) *Config {
	t.Helper()
	return &Config{
		Dataset:        "tank/photos",
		SnapshotPrefix: "photowatch",
		Threshold:      1,
		KeepDays:       14,
		ReportDir:      reportDir,
		StateDir:       stateDir,
		ZfsPath:        fakeZfs(t, "tank/photos", source.Mountpoint, deletedPath),
		ThumbnailDir:   thumbDir,
		ThumbnailMax:   24,
		ThumbnailPx:    320,
		ThumbnailMaxMB: 512,
		KeepDaysReport: 365,
	}
}

// At 08:15 files disappear and a notification goes out with the path to
// restore-<today>.sh in it. If the watch runs again at 10:30 — over an entirely
// different set — it may not replace the script, the list, the report and the
// thumbnails of 08:15: the message on the phone still points at them.
func TestSecondRunDoesNotOverwriteTheArtifactsOfTheFirst(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	morning := putImage(t, source, "From the old box/PICT0033 (5).JPG", 640, 480, "jpeg")
	afternoon := putImage(t, source, "Croatia/IMG_4471.jpg", 320, 240, "jpeg")
	reportDir, thumbDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	log, _ := captureLog()

	p1, plan1, err := run(context.Background(), testConfig(t, source, morning.Raw, reportDir, thumbDir, stateDir), log, false)
	if err != nil {
		t.Fatalf("first run returned an error: %v", err)
	}
	runAftercare(context.Background(), log, plan1)

	today := time.Now().Format("2006-01-02")
	if filepath.Base(p1.RestoreScript) != "restore-"+today+".sh" {
		t.Fatalf("first run wrote %q", p1.RestoreScript)
	}
	// What the morning run left on disk, byte for byte.
	before := map[string][]byte{}
	for _, path := range []string{
		p1.RestoreScript,
		filepath.Join(reportDir, "restore-"+today+".list"),
		p1.Report,
		filepath.Join(p1.Thumbnails, "001-PICT0033__5_.jpg"),
		filepath.Join(p1.Thumbnails, "report.txt"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the first run did not leave %s behind: %v", path, err)
		}
		before[path] = data
	}

	p2, plan2, err := run(context.Background(), testConfig(t, source, afternoon.Raw, reportDir, thumbDir, stateDir), log, false)
	if err != nil {
		t.Fatalf("second run returned an error: %v", err)
	}
	runAftercare(context.Background(), log, plan2)

	// The second run writes under a name of its own …
	for what, got := range map[string]string{
		"restore script": p2.RestoreScript,
		"report":         p2.Report,
		"day directory":  p2.Thumbnails,
	} {
		if got == "" {
			t.Errorf("the second run names no %s", what)
			continue
		}
		if !strings.Contains(filepath.Base(got), today+"-2") {
			t.Errorf("second run wrote %s to %q; the stamp %s-2 belongs in it", what, got, today)
		}
		if _, err := os.Stat(got); err != nil {
			t.Errorf("%s of the second run is not there: %v", what, err)
		}
	}
	// … and says in the message that it is the second of today, otherwise the
	// notification points at names with a sequence number without anyone knowing
	// why.
	if !strings.Contains(p2.SideIssues, "second run today") {
		t.Errorf("side_issues = %q; the second run of a day should report that", p2.SideIssues)
	}

	// And most importantly: everything of the morning run is unchanged.
	for path, want := range before {
		now, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s of the first run disappeared: %v", path, err)
			continue
		}
		if !bytes.Equal(now, want) {
			t.Errorf("%s of the first run was changed by the second run", path)
		}
	}
}

// -dry-run promised "create, delete or send nothing", but wrote the restore
// script, the list and the thumbnails in their real place and deleted the
// thumbnails of that morning's real run in the process. A dry run should do
// everything in its own directory and touch nothing outside it.
func TestDryRunDoesNotTouchTheRealArtifacts(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	photo := putImage(t, source, "From the old box/PICT0033 (5).JPG", 640, 480, "jpeg")
	reportDir, thumbDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	log, _ := captureLog()

	// First this morning's real run: it puts down the script, the list, the
	// report and the thumbnails the notification points at.
	cfg := testConfig(t, source, photo.Raw, reportDir, thumbDir, stateDir)
	p1, plan1, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("real run returned an error: %v", err)
	}
	runAftercare(context.Background(), log, plan1)
	// And an old report a dry run may mention but not throw away.
	makeDatedFiles(t, reportDir, "deleted-%s.txt", 20, time.Date(2020, 1, 20, 8, 0, 0, 0, time.Local))

	record := func() map[string][]byte {
		t.Helper()
		out := map[string][]byte{}
		for _, root := range []string{reportDir, thumbDir, p1.Thumbnails} {
			items, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("directory %s not readable: %v", root, err)
			}
			for _, item := range items {
				path := filepath.Join(root, item.Name())
				if item.IsDir() {
					out[path] = nil
					continue
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%s not readable: %v", path, err)
				}
				out[path] = data
			}
		}
		return out
	}
	real := record()

	// Then the dry run.
	pd, planDry, err := run(context.Background(), cfg, log, true)
	if err != nil {
		t.Fatalf("dry run returned an error: %v", err)
	}
	runAftercare(context.Background(), log, planDry)

	// Everything of the real run is still there, byte for byte, and nothing was
	// added except the dry-run directory itself.
	after := record()
	for path, want := range real {
		got, present := after[path]
		if !present {
			t.Errorf("the dry run deleted %s", path)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("the dry run changed %s", path)
		}
	}
	for path := range after {
		if _, present := real[path]; !present && filepath.Base(path) != dryRunDir {
			t.Errorf("the dry run created %s outside its own directory", path)
		}
	}

	// And the dry run *did* put something down, because otherwise there is
	// nothing to check: that is the reason it does not simply write nothing.
	dryReportDir := filepath.Join(reportDir, dryRunDir)
	dryDayDir := filepath.Join(thumbDir, dryRunDir)
	if pd.RestoreScript != filepath.Join(dryReportDir, "restore.sh") {
		t.Errorf("dry run wrote the restore script to %q", pd.RestoreScript)
	}
	if pd.Report != filepath.Join(dryReportDir, "deleted.txt") {
		t.Errorf("dry run wrote the report to %q", pd.Report)
	}
	if pd.Thumbnails != dryDayDir {
		t.Errorf("dry run pointed the thumbnails at %q, want %q", pd.Thumbnails, dryDayDir)
	}
	for _, path := range []string{
		filepath.Join(dryReportDir, "restore.sh"),
		filepath.Join(dryReportDir, "restore.list"),
		filepath.Join(dryReportDir, "deleted.txt"),
		filepath.Join(dryDayDir, "001-PICT0033__5_.jpg"),
		filepath.Join(dryDayDir, "report.txt"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the dry run did not leave %s behind: %v", path, err)
		}
	}
	content, err := os.ReadFile(filepath.Join(dryReportDir, "deleted.txt"))
	if err != nil {
		t.Fatalf("dry-run report not readable: %v", err)
	}
	if !strings.Contains(string(content), "NOTE: dry run") {
		t.Error("the report of a dry run does not say that it is one")
	}

	// A second dry run replaces its own result and does not grow.
	_, planDry2, err := run(context.Background(), cfg, log, true)
	if err != nil {
		t.Fatalf("second dry run returned an error: %v", err)
	}
	runAftercare(context.Background(), log, planDry2)
	items, err := os.ReadDir(dryReportDir)
	if err != nil {
		t.Fatalf("dry-run directory not readable: %v", err)
	}
	if len(items) != 3 {
		names := []string{}
		for _, i := range items {
			names = append(names, i.Name())
		}
		t.Errorf("after two dry runs the dry-run directory holds %v, want three files", names)
	}
}

// What a run ran into after its notification travels via status.json to the
// *next* run. If that next run strands early — `zfs list` hiccups — it does
// write a fresh status.json, and without this carry-over yesterday's side issue
// would disappear from every chain.
func TestRunThatStrandsEarlyKeepsYesterdaysAftercare(t *testing.T) {
	stateDir := t.TempDir()
	yesterday := &AftercareResult{CleanedUp: 3, ArtifactsMB: 42, SideIssues: "cleanup failed: /var/log/photowatch cannot be walked"}
	writeTestState(t, stateDir, StateFile{
		Written:   time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
		Reported:  true,
		Payload:   Payload{Version: 1, Status: "ok"},
		Aftercare: yesterday,
	})
	cfg := &Config{StateDir: stateDir}
	log, _ := captureLog()

	// Stranded early: run() returned no aftercare plan.
	sf := stateAfterRun(log, cfg, Payload{Status: "error"}, true, nil)
	if sf.Aftercare == nil {
		t.Fatal("yesterday's aftercare was not carried over; that side issue has then disappeared from every chain")
	}
	if sf.Aftercare.SideIssues != yesterday.SideIssues || sf.Aftercare.ArtifactsMB != 42 {
		t.Errorf("carried-over aftercare = %+v, want %+v", *sf.Aftercare, *yesterday)
	}

	// And the ordinary path: if the run did get far enough to have an aftercare
	// plan, yesterday's side issue is already in today's payload and it must
	// specifically *not* stay here — otherwise it repeats every day.
	sf = stateAfterRun(log, cfg, Payload{Status: "ok"}, true, &AftercarePlan{})
	if sf.Aftercare != nil {
		t.Errorf("aftercare = %+v; on an ordinary run it should come from this run, not from yesterday", *sf.Aftercare)
	}
}

// artifacts_mb is measured in the aftercare (after the cleanup, after the
// notification) and travels in the message of the *next* run. A directory that
// cannot be measured may not pass silently as 0.
func TestArtifactsMBIsMeasuredInTheAftercareAndReportedTomorrow(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	photo := putImage(t, source, "From the old box/PICT0033 (5).JPG", 64, 48, "jpeg")
	reportDir, thumbDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	log, _ := captureLog()

	// Three megabytes of old reports. If run() measured it itself, that number
	// would be in the payload of this run right away.
	if err := os.WriteFile(filepath.Join(reportDir, "deleted-2026-01-01.txt"), make([]byte, 3<<20), 0o640); err != nil {
		t.Fatalf("large test file cannot be written: %v", err)
	}

	cfg := testConfig(t, source, photo.Raw, reportDir, thumbDir, stateDir)
	p1, plan1, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if p1.ArtifactsMB != 0 {
		t.Errorf("artifacts_mb = %d in the payload of the first run; that number should come from the aftercare of the *previous* run and not be measured before the notification", p1.ArtifactsMB)
	}
	result := runAftercare(context.Background(), log, plan1)
	if result.ArtifactsMB < 3 {
		t.Errorf("aftercare measured %d MB, want at least 3", result.ArtifactsMB)
	}

	// The next run reports yesterday's number.
	writeTestState(t, stateDir, StateFile{
		Written:   time.Now().Format(time.RFC3339),
		Reported:  true,
		Payload:   p1,
		Aftercare: &result,
	})
	p2, _, err := run(context.Background(), testConfig(t, source, photo.Raw, reportDir, thumbDir, stateDir), log, false)
	if err != nil {
		t.Fatalf("second run returned an error: %v", err)
	}
	if p2.ArtifactsMB != result.ArtifactsMB {
		t.Errorf("artifacts_mb = %d in the message of the next run, want %d from the aftercare", p2.ArtifactsMB, result.ArtifactsMB)
	}
}

// A directory that cannot be measured used to count silently as 0, at DEBUG
// level. That made the 800 MB threshold impossible to reach by definition.
func TestArtifactsMBReportsAnUnmeasurableDirectory(t *testing.T) {
	log, buf := captureLog()
	reportDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(reportDir, "deleted-2026-01-01.txt"), make([]byte, 2<<20), 0o640); err != nil {
		t.Fatalf("test file cannot be written: %v", err)
	}
	mb, err := artifactsMB(log, reportDir, filepath.Join(reportDir, "does-not-exist"))
	if err == nil {
		t.Fatal("an unmeasurable thumbnail directory produces no error; then the number stays silently too low")
	}
	if mb != 2 {
		t.Errorf("measured %d MB, want 2 (what *could* be measured)", mb)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("there is no WARN in the journal:\n%s", buf.String())
	}
}

// On a day when only videos, raw files or sidecars disappear there is no
// thumbnail to make at all, and then the *reason* is the only thing there is to
// say about the thumbnails. If MakeThumbnails did not run, that reason ended up
// nowhere: the report got an empty ThumbnailResult and stayed silent.
func TestReportNamesTheReasonWhenThereIsNoThumbnailToMake(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	movie := putFile(t, source, "From the old box/christmas 2004.mov", []byte("not an image"))
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	stateDir := t.TempDir()

	cfg := &Config{
		Dataset:        "tank/photos",
		SnapshotPrefix: "photowatch",
		Threshold:      1,
		KeepDays:       14,
		ReportDir:      reportDir,
		StateDir:       stateDir,
		ZfsPath:        fakeZfs(t, "tank/photos", source.Mountpoint, movie.Raw),
		ThumbnailDir:   thumbDir,
		ThumbnailMax:   24,
		ThumbnailPx:    320,
		ThumbnailMaxMB: 512,
		KeepDaysReport: 365,
	}

	log, _ := captureLog()
	p, plan, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if p.ThumbnailsPlanned != 0 || p.Thumbnails != "" {
		t.Errorf("the payload promises thumbnails of a .mov: planned=%d dir=%q", p.ThumbnailsPlanned, p.Thumbnails)
	}
	runAftercare(context.Background(), log, plan)

	data, err := os.ReadFile(p.Report)
	if err != nil {
		t.Fatalf("report not readable: %v", err)
	}
	report := string(data)
	if !strings.Contains(report, "Why some files have no thumbnail") {
		t.Errorf("the report does not explain why there is no thumbnail:\n%s", report)
	}
	if !strings.Contains(report, "not jpg/png/gif") {
		t.Errorf("the reason 'not jpg/png/gif' is not in the report:\n%s", report)
	}
	if !strings.Contains(report, "Thumbnails:         none —") {
		t.Errorf("the header block stays silent about the thumbnails:\n%s", report)
	}
}

// The same path, but now the plan strands on THUMBNAIL_MAX_MB. That message
// does reach the payload (it is known before the notification); without the
// plan in the aftercare it was not in the report, while WriteReport has a
// branch for it.
func TestReportNamesTheLimitWhenTheThumbnailDirIsFull(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	photo := putImage(t, source, "From the old box/PICT0033 (5).JPG", 320, 240, "jpeg")
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	stateDir := t.TempDir()
	// One megabyte in the thumbnail directory and a limit of 1 MB: the plan may
	// then promise nothing.
	if err := os.WriteFile(filepath.Join(thumbDir, "filler.bin"), make([]byte, 1<<20), 0o640); err != nil {
		t.Fatalf("filler cannot be written: %v", err)
	}

	cfg := &Config{
		Dataset:        "tank/photos",
		SnapshotPrefix: "photowatch",
		Threshold:      1,
		KeepDays:       14,
		ReportDir:      reportDir,
		StateDir:       stateDir,
		ZfsPath:        fakeZfs(t, "tank/photos", source.Mountpoint, photo.Raw),
		ThumbnailDir:   thumbDir,
		ThumbnailMax:   24,
		ThumbnailPx:    320,
		ThumbnailMaxMB: 1,
		KeepDaysReport: 365,
	}

	log, _ := captureLog()
	p, plan, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if !strings.Contains(p.SideIssues, "limit 1") {
		t.Errorf("the payload does not name the limit: %q", p.SideIssues)
	}
	runAftercare(context.Background(), log, plan)

	data, err := os.ReadFile(p.Report)
	if err != nil {
		t.Fatalf("report not readable: %v", err)
	}
	if !strings.Contains(string(data), "Thumbnails:         none — thumbnail directory is") {
		t.Errorf("the report stays silent about the full thumbnail directory:\n%s", data)
	}
}

// The explanation of the sequence number may not fall out of the message. The
// aftercare of yesterday often leaves a long side issue behind; if that comes
// first, maxFailureTextLength cuts off exactly the sentence that says why
// today's notification points at 2026-09-01-2.
func TestSequenceNumberExplanationSurvivesALongSideIssue(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	photo := putImage(t, source, "From the old box/PICT0033 (5).JPG", 64, 48, "jpeg")
	reportDir, thumbDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()

	// Four items from yesterday's aftercare, together well over the 200
	// characters side_issues is truncated at. This is the text as it sits in
	// status.json.
	previous := "cleanup failed: /var/log/photowatch cannot be walked; " +
		"thumbnails could not be made; report could not be written; " +
		"thumbnail directory /mnt/tank/photowatch cannot be measured"
	writeTestState(t, stateDir, StateFile{
		Written:   time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
		Reported:  true,
		Payload:   Payload{Version: 1, Status: "ok"},
		Aftercare: &AftercareResult{SideIssues: previous},
	})

	// The set of today's first run is already there; this run therefore has to
	// move to sequence number 2.
	stamp := time.Now().Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(reportDir, "deleted-"+stamp+".txt"), []byte("from this morning\n"), 0o640); err != nil {
		t.Fatalf("report of the first run cannot be written: %v", err)
	}

	cfg := testConfig(t, source, photo.Raw, reportDir, thumbDir, stateDir)
	log, _ := captureLog()
	p, _, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if !strings.Contains(p.Report, stamp+"-2") {
		t.Fatalf("this run did not move to a sequence number: report=%q", p.Report)
	}
	if !strings.Contains(p.SideIssues, "these are called "+stamp+"-2") {
		t.Errorf("the explanation of the sequence number is not in the message:\n%q", p.SideIssues)
	}
	// The case is only sharp when the text is really truncated; without that
	// check the test would pass as soon as the items happen to be short enough.
	// shortText appends one "…", hence the +1.
	runes := []rune(p.SideIssues)
	if len(runes) > maxFailureTextLength+1 {
		t.Errorf("side_issues is %d characters, limit %d", len(runes), maxFailureTextLength)
	}
	if !strings.HasSuffix(p.SideIssues, "…") {
		t.Errorf("the message is not truncated; then this test does not measure the case:\n%q", p.SideIssues)
	}
}

// If ChooseArtifactSlot strands, there is no day directory with thumbnails
// either. The side issue only named the report and the restore script, while
// the ERROR line in the journal did say it in full.
func TestNoSlotForArtifactsAlsoNamesTheThumbnails(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash on this machine; the fake zfs is a bash script")
	}
	source, _ := testSource(t)
	photo := putImage(t, source, "From the old box/PICT0033 (5).JPG", 64, 48, "jpeg")
	reportDir, thumbDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()

	// Every stamp of today taken: then there is no slot left. One of the four
	// names per set is enough, because slotFree looks at all four.
	today := time.Now().Format("2006-01-02")
	for n := 1; n <= maxRunsPerDay; n++ {
		stamp := today
		if n > 1 {
			stamp = fmt.Sprintf("%s-%d", today, n)
		}
		path := filepath.Join(reportDir, "deleted-"+stamp+".txt")
		if err := os.WriteFile(path, []byte("taken\n"), 0o640); err != nil {
			t.Fatalf("set %s cannot be put down: %v", stamp, err)
		}
	}

	cfg := testConfig(t, source, photo.Raw, reportDir, thumbDir, stateDir)
	log, buf := captureLog()
	p, plan, err := run(context.Background(), cfg, log, false)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if !strings.Contains(p.SideIssues, "no slot for report, restore script and thumbnails") {
		t.Errorf("the side issue does not name the thumbnails: %q", p.SideIssues)
	}
	// And it is really true: no path to a day directory appears in the message,
	// and the aftercare makes none.
	if p.Thumbnails != "" || p.Report != "" {
		t.Errorf("the notification points at artifacts that will not come: thumbnails=%q report=%q", p.Thumbnails, p.Report)
	}
	runAftercare(context.Background(), log, plan)
	items, err := os.ReadDir(thumbDir)
	if err != nil {
		t.Fatalf("thumbnail directory not readable: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("there is something in the thumbnail directory after all: %v", items)
	}
	if !strings.Contains(buf.String(), "no slot for the artifacts of this run") {
		t.Errorf("the journal does not explain what went wrong:\n%s", buf.String())
	}
}
