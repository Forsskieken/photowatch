package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The tests in this file belong to cleanup.go: all file deletion of this
// program. At least one test per guardrail, following the example of
// retention_test.go — that is the reason that code lives in one place.

// A SIGKILL, an OOM or a power cut between CreateTemp and Rename leaves a
// temporary file behind; nothing else cleans that up later. The next write does
// it, but only for names that match our own pattern exactly and only when they
// are older than a day.
func TestWriteReportCleansOldLeftovers(t *testing.T) {
	dir := t.TempDir()
	longAgo := time.Now().Add(-3 * 24 * time.Hour)

	create := func(name string, ts time.Time) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("half written"), 0o640); err != nil {
			t.Fatalf("test file %s cannot be written: %v", name, err)
		}
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatalf("time of %s cannot be set: %v", name, err)
		}
		return path
	}
	oldLeftover := create(".photowatch-3820156743", longAgo)
	freshLeftover := create(".photowatch-118829933", time.Now())
	// A file somebody named that way themselves: no digits after the dash, so
	// not from os.CreateTemp and therefore not ours.
	byHand := create(".photowatch-notes", longAgo)
	other := create("deleted-2020-01-01.txt", longAgo)

	subDir := filepath.Join(dir, ".photowatch-4711")
	if err := os.Mkdir(subDir, 0o750); err != nil {
		t.Fatalf("test directory cannot be created: %v", err)
	}
	if err := os.Chtimes(subDir, longAgo, longAgo); err != nil {
		t.Fatalf("time of the test directory cannot be set: %v", err)
	}
	// A symlink with a matching name pointing outside the directory: it may
	// never be followed or deleted.
	target := filepath.Join(t.TempDir(), "do-not-touch.txt")
	if err := os.WriteFile(target, []byte("stays"), 0o640); err != nil {
		t.Fatalf("target cannot be written: %v", err)
	}
	link := filepath.Join(dir, ".photowatch-999999")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink cannot be created: %v", err)
	}

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	if _, err := WriteReport(log, dir, sampleReportData(t, 2)); err != nil {
		t.Fatalf("writing the report failed: %v", err)
	}

	if _, err := os.Stat(oldLeftover); !os.IsNotExist(err) {
		t.Errorf("the old leftover %s is still there (err=%v)", oldLeftover, err)
	}
	for _, stays := range []string{freshLeftover, byHand, other, subDir, target} {
		if _, err := os.Stat(stays); err != nil {
			t.Errorf("%s should have stayed: %v", stays, err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink %s should have stayed: %v", link, err)
	}
	if !strings.Contains(buf.String(), "leftover of an aborted write cleaned up") {
		t.Errorf("the cleanup is not in the log: %s", buf.String())
	}
}

// An unreadable directory may never hold up writing the report: the cleanup is
// a side issue, the notification points at this file.
func TestCleanupDoesNotHoldUpWriting(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	dir := t.TempDir()
	removeStaleTemps(log, filepath.Join(dir, "does-not-exist"), time.Now())
	if !strings.Contains(buf.String(), "cannot be walked") {
		t.Errorf("an unreadable directory should give a warning: %s", buf.String())
	}
	if _, err := WriteReport(log, dir, sampleReportData(t, 1)); err != nil {
		t.Fatalf("writing the report failed: %v", err)
	}
}

// --- The seven guardrails of CleanArtifacts ---

// captureLog gives a logger writing to a buffer, so that a test can check
// *that* a warning was given. Silence is a bug here: when something stays, the
// reason has to be visible.
func captureLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// makeDatedFiles puts down n files with daily descending dates in the name, the
// newest on end. The pattern holds one %s for the date.
func makeDatedFiles(t *testing.T, dir, pattern string, n int, end time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf(pattern, end.AddDate(0, 0, -i).Format("2006-01-02"))
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content"), 0o640); err != nil {
			t.Fatalf("test file %s cannot be written: %v", name, err)
		}
	}
}

func countFiles(t *testing.T, dir, suffix string) int {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("directory %s not readable: %v", dir, err)
	}
	n := 0
	for _, item := range items {
		if strings.HasSuffix(item.Name(), suffix) {
			n++
		}
	}
	return n
}

func testJob(reportDir, thumbnailDir string) CleanupJob {
	return CleanupJob{
		ReportDir:        reportDir,
		ThumbnailDir:     thumbnailDir,
		StateDir:         "/var/lib/photowatch",
		Mountpoint:       "/mnt/tank/photos",
		KeepDaysReport:   365,
		ArtifactKeepDays: 16,
	}
}

// Guardrail 3: only names that match our own pattern exactly. Whoever renames a
// report to ...-IMPORTANT.txt has kept it forever, and that is exactly the
// intention.
func TestCleanArtifactsLeavesForeignNames(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)
	old := now.AddDate(-1, 0, 0)

	makeDatedFiles(t, dir, "restore-%s.sh", 6, old)
	stayers := []string{
		"deleted-2025-01-04-IMPORTANT.txt", // renamed by a human
		"restore-2025-01-04.sh.bak",        // copy
		"restore-20250104.sh",              // no dashes in the date
		"notes.txt",
		"restore-2025-13-45.sh", // not a valid date
	}
	for _, name := range stayers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o640); err != nil {
			t.Fatalf("test file cannot be written: %v", err)
		}
	}

	log, _ := captureLog()
	res, err := CleanArtifacts(log, testJob(dir, ""), now)
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	// Six scripts, the newest three stay: three go.
	if res.Scripts != 3 {
		t.Errorf("cleaned scripts = %d, want 3", res.Scripts)
	}
	for _, name := range stayers {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should have stayed: %v", name, err)
		}
	}
}

// Guardrail 6: the newest always stay, no matter how old. If the clock jumps
// forward, everything suddenly looks too old.
func TestCleanArtifactsKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	today := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)
	makeDatedFiles(t, dir, "deleted-%s.txt", 20, today)
	makeDatedFiles(t, dir, "restore-%s.sh", 6, today)
	makeDatedFiles(t, dir, "restore-%s.list", 6, today)

	// The clock is a hundred days ahead: everything is "too old" now.
	o := testJob(dir, "")
	o.KeepDaysReport = 14
	log, _ := captureLog()
	res, err := CleanArtifacts(log, o, today.AddDate(0, 0, 100))
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if got := countFiles(t, dir, ".txt"); got != MinimumReports {
		t.Errorf("%d reports remain, want %d", got, MinimumReports)
	}
	if got := countFiles(t, dir, ".sh"); got != MinimumRestore {
		t.Errorf("%d restore scripts remain, want %d", got, MinimumRestore)
	}
	if got := countFiles(t, dir, ".list"); got != MinimumRestore {
		t.Errorf("%d lists remain, want %d", got, MinimumRestore)
	}
	if res.Total() != 6+3+3 {
		t.Errorf("total cleaned = %d, want 12", res.Total())
	}
}

// Guardrail 7: at most twenty deletions per run, with a WARN about the backlog.
// The rest follows on the days after.
func TestCleanArtifactsMaximumPerRun(t *testing.T) {
	dir := t.TempDir()
	today := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)
	makeDatedFiles(t, dir, "deleted-%s.txt", 60, today)

	o := testJob(dir, "")
	o.KeepDaysReport = 14
	log, buf := captureLog()
	res, err := CleanArtifacts(log, o, today.AddDate(0, 0, 100))
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if res.Total() != MaxCleanupPerRun {
		t.Errorf("cleaned = %d, want exactly %d", res.Total(), MaxCleanupPerRun)
	}
	if !res.Backlog {
		t.Error("the backlog was not reported")
	}
	if !strings.Contains(buf.String(), "the rest follows in the coming days") {
		t.Errorf("there is no warning about the backlog in the log: %s", buf.String())
	}
	if got := countFiles(t, dir, ".txt"); got != 40 {
		t.Errorf("%d reports remain, want 40", got)
	}
}

// Guardrail 4: if a day directory holds even one file we do not recognize, the
// whole directory stays and a WARN follows. A human put something there then.
func TestCleanArtifactsLeavesDayDirWithForeignFile(t *testing.T) {
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	today := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)

	makeDayDir := func(date string, files map[string]string) string {
		path := filepath.Join(thumbDir, date)
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatalf("day directory cannot be created: %v", err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o640); err != nil {
				t.Fatalf("file in the day directory cannot be written: %v", err)
			}
		}
		return path
	}
	// Three recent day directories stay in any case (guardrail 6).
	for i := 0; i < 3; i++ {
		makeDayDir(today.AddDate(0, 0, -i).Format("2006-01-02"), map[string]string{"report.txt": "x"})
	}
	clean := makeDayDir("2025-01-02", map[string]string{"report.txt": "x", "001-IMG_4471.jpg": "x"})
	dirty := makeDayDir("2025-01-01", map[string]string{"report.txt": "x", "notes.txt": "from a human"})

	log, buf := captureLog()
	res, err := CleanArtifacts(log, testJob(reportDir, thumbDir), today)
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if res.DayDirs != 1 {
		t.Errorf("cleaned day directories = %d, want 1", res.DayDirs)
	}
	if _, err := os.Stat(clean); !os.IsNotExist(err) {
		t.Errorf("the clean day directory is still there (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dirty, "notes.txt")); err != nil {
		t.Errorf("the day directory with a foreign file should have stayed entirely: %v", err)
	}
	if !strings.Contains(buf.String(), "did not write itself") {
		t.Errorf("there is no warning about the foreign day directory in the log: %s", buf.String())
	}
}

// A symlink that looks like a day directory is not followed and not deleted.
func TestCleanArtifactsFollowsNoSymlink(t *testing.T) {
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	elsewhere := t.TempDir()
	today := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)

	if err := os.WriteFile(filepath.Join(elsewhere, "do-not-touch.txt"), []byte("stays"), 0o640); err != nil {
		t.Fatalf("target cannot be written: %v", err)
	}
	link := filepath.Join(thumbDir, "2025-01-01")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("symlink cannot be created: %v", err)
	}
	// And a symlink that looks like a report.
	reportLink := filepath.Join(reportDir, "deleted-2025-01-01.txt")
	if err := os.Symlink(filepath.Join(elsewhere, "do-not-touch.txt"), reportLink); err != nil {
		t.Fatalf("symlink cannot be created: %v", err)
	}
	makeDatedFiles(t, reportDir, "deleted-%s.txt", 20, today)

	o := testJob(reportDir, thumbDir)
	o.KeepDaysReport = 14
	log, _ := captureLog()
	if _, err := CleanArtifacts(log, o, today.AddDate(0, 0, 100)); err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink to a directory should have stayed: %v", err)
	}
	if _, err := os.Lstat(reportLink); err != nil {
		t.Errorf("the symlink with a report name should have stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "do-not-touch.txt")); err != nil {
		t.Errorf("the file outside the directory was touched: %v", err)
	}
}

// The validation before anything at all is deleted: a target directory that is
// a system directory, or that lies in the watched archive, produces an error
// and deletes nothing.
func TestCleanArtifactsRefusesDangerousDirectories(t *testing.T) {
	cases := []struct {
		name         string
		reportDir    string
		thumbnailDir string
	}{
		{"var", "/var", ""},
		{"var-log", "/var/log", ""},
		{"mnt", "/mnt", ""},
		{"root", "/", ""},
		{"relative", "log/photowatch", ""},
		{"state dir", "/var/lib/photowatch", ""},
		{"inside the archive", "/var/log/photowatch", "/mnt/tank/photos/thumbs"},
		{"the archive itself", "/var/log/photowatch", "/mnt/tank/photos"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log, _ := captureLog()
			o := testJob(c.reportDir, c.thumbnailDir)
			res, err := CleanArtifacts(log, o, time.Now())
			if err == nil {
				t.Fatalf("%s was accepted as a target directory", c.reportDir+" "+c.thumbnailDir)
			}
			if res.Total() != 0 {
				t.Errorf("something was deleted anyway: %d", res.Total())
			}
		})
	}
}

// -dry-run deletes nothing but does report what it would do.
func TestCleanArtifactsDryRunDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	thumbDir := t.TempDir()
	today := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)
	makeDatedFiles(t, dir, "deleted-%s.txt", 20, today)
	for i := 0; i < 5; i++ {
		date := today.AddDate(0, 0, -i).Format("2006-01-02")
		if err := os.Mkdir(filepath.Join(thumbDir, date), 0o750); err != nil {
			t.Fatalf("day directory cannot be created: %v", err)
		}
	}

	o := testJob(dir, thumbDir)
	o.KeepDaysReport = 14
	o.DryRun = true
	log, _ := captureLog()
	res, err := CleanArtifacts(log, o, today.AddDate(0, 0, 100))
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if res.Total() != 6+2 {
		t.Errorf("dry run reports %d items, want 8 (6 reports and 2 day directories)", res.Total())
	}
	if got := countFiles(t, dir, ".txt"); got != 20 {
		t.Errorf("a dry run deleted %d reports; that is not allowed", 20-got)
	}
	if got := len(mustReadDir(t, thumbDir)); got != 5 {
		t.Errorf("a dry run deleted day directories: %d of 5 remain", got)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("directory %s not readable: %v", dir, err)
	}
	return items
}

// Guardrail 5: the age comes from the name and not from mtime, and a date in
// the future is not a date to clean up.
func TestCleanArtifactsIgnoresMtimeAndTheFuture(t *testing.T) {
	dir := t.TempDir()
	today := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)
	makeDatedFiles(t, dir, "restore-%s.sh", 6, today)

	// An old file with a fresh mtime must simply go (the name counts), and a
	// name in the future stays, no matter how old the mtime is.
	future := filepath.Join(dir, "restore-2099-01-01.sh")
	if err := os.WriteFile(future, []byte("x"), 0o640); err != nil {
		t.Fatalf("test file cannot be written: %v", err)
	}
	longAgo := today.AddDate(-5, 0, 0)
	if err := os.Chtimes(future, longAgo, longAgo); err != nil {
		t.Fatalf("time cannot be set: %v", err)
	}
	oldest := filepath.Join(dir, today.AddDate(0, 0, -5).Format("restore-2006-01-02.sh"))
	if err := os.Chtimes(oldest, today, today); err != nil {
		t.Fatalf("time cannot be set: %v", err)
	}

	log, buf := captureLog()
	// Three weeks later: the six scripts are all older than 16 days by then.
	res, err := CleanArtifacts(log, testJob(dir, ""), today.AddDate(0, 0, 21))
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if res.Scripts != 3 {
		t.Errorf("cleaned scripts = %d, want 3", res.Scripts)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("the oldest script is still there despite its fresh mtime (err=%v)", err)
	}
	if _, err := os.Stat(future); err != nil {
		t.Errorf("a script with a date in the future should have stayed: %v", err)
	}
	if !strings.Contains(buf.String(), "date in the future") {
		t.Errorf("there is no warning about the future date in the log: %s", buf.String())
	}
}

// An empty or missing directory is not an error: then nothing has ever been
// written.
func TestCleanArtifactsOnEmptyDirectories(t *testing.T) {
	dir := t.TempDir()
	log, _ := captureLog()
	res, err := CleanArtifacts(log, testJob(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "neither")), time.Now())
	if err != nil {
		t.Fatalf("a missing directory should not be an error: %v", err)
	}
	if res.Total() != 0 {
		t.Errorf("something was deleted in an empty directory: %d", res.Total())
	}
}

func TestDirSizeMB(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), make([]byte, 3<<20), 0o640); err != nil {
		t.Fatalf("test file cannot be written: %v", err)
	}
	sub := filepath.Join(dir, "2026-08-31")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatalf("subdirectory cannot be created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "001-a.jpg"), make([]byte, 2<<20), 0o640); err != nil {
		t.Fatalf("test file cannot be written: %v", err)
	}
	mb, err := DirSizeMB(dir)
	if err != nil {
		t.Fatalf("size cannot be determined: %v", err)
	}
	if mb != 5 {
		t.Errorf("size = %d MB, want 5", mb)
	}
}

// If `zfs get mountpoint` falls away, the safeguard "THUMBNAIL_DIR is not
// inside the watched archive" cannot be executed. That directory is then not
// cleaned this run — otherwise a misconfigured THUMBNAIL_DIR would let the
// cleanup descend into the photo archive, exactly on the run where the check is
// missing. The report directory does continue.
func TestCleanArtifactsSkipsThumbnailsWithoutMountpoint(t *testing.T) {
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	today := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)
	makeDatedFiles(t, reportDir, "deleted-%s.txt", 20, today)
	for i := 0; i < 5; i++ {
		date := today.AddDate(0, 0, -i-100).Format("2006-01-02")
		if err := os.Mkdir(filepath.Join(thumbDir, date), 0o750); err != nil {
			t.Fatalf("day directory cannot be created: %v", err)
		}
	}

	o := testJob(reportDir, thumbDir)
	o.Mountpoint = "" // `zfs get mountpoint` failed this run
	o.KeepDaysReport = 14
	log, buf := captureLog()
	res, err := CleanArtifacts(log, o, today.AddDate(0, 0, 100))
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if res.DayDirs != 0 {
		t.Errorf("%d day directories were cleaned while the mountpoint was unknown", res.DayDirs)
	}
	if got := len(mustReadDir(t, thumbDir)); got != 5 {
		t.Errorf("%d of the 5 day directories remain; without a mountpoint nothing may go", got)
	}
	// The report directory needs no mountpoint and *is* cleaned.
	if res.Reports == 0 {
		t.Error("the report directory was not cleaned; it does not depend on the mountpoint")
	}
	if !strings.Contains(res.Message, "mountpoint") {
		t.Errorf("the payload does not say that the thumbnail directory was skipped: %q", res.Message)
	}
	if !strings.Contains(buf.String(), "mountpoint of the dataset is unknown") {
		t.Errorf("the journal does not explain why nothing was cleaned: %s", buf.String())
	}
}

// Several kinds can go wrong at once and there is one field in the payload.
// Then both belong in it — not only the last one.
func TestCleanArtifactsKeepsAllMessages(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a directory with mode 000 is simply readable")
	}
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	for _, dir := range []string{reportDir, thumbDir} {
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatalf("permissions of %s cannot be set: %v", dir, err)
		}
		// Restore, otherwise t.TempDir cannot do its own cleanup.
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}

	log, _ := captureLog()
	res, err := CleanArtifacts(log, testJob(reportDir, thumbDir), time.Now())
	if err != nil {
		t.Fatalf("an unreadable directory should not be a hard error: %v", err)
	}
	for _, want := range []string{reportDir, thumbDir} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("the message %q does not name %s; only the last failure survives", res.Message, want)
		}
	}
}

// The same failure twenty times is one problem, not twenty lines in a message
// on a phone screen.
func TestCleanupMessagesWithoutRepetition(t *testing.T) {
	var list []string
	addCleanupMessage(&list, "a")
	addCleanupMessage(&list, "a")
	addCleanupMessage(&list, "b")
	if len(list) != 2 {
		t.Fatalf("list = %v, want two different messages", list)
	}
	for i := 0; i < 10; i++ {
		addCleanupMessage(&list, fmt.Sprintf("message %d", i))
	}
	if len(list) != maxCleanupMessages+1 {
		t.Fatalf("list = %v, want %d messages plus the pointer to the journal", list, maxCleanupMessages)
	}
	if !strings.Contains(list[len(list)-1], "journal") {
		t.Errorf("the last line does not point at the journal: %v", list)
	}
}

// The artifacts of the second and later run of a day get a stamp with a
// sequence number (see ArtifactSlot). The cleanup must recognize that shape —
// otherwise they stay forever — and it may not recognize anything that does not
// match it.
func TestCleanArtifactsRecognizesTheStampOfASecondRun(t *testing.T) {
	dir := t.TempDir()
	thumbDir := t.TempDir()
	now := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)
	old := now.AddDate(-1, 0, 0)

	// Four old days, each with two runs: eight sets. The newest three restore
	// scripts stay (guardrail 6), the rest is a year old.
	for i := 0; i < 4; i++ {
		date := old.AddDate(0, 0, -i).Format("2006-01-02")
		for _, name := range []string{
			"deleted-" + date + ".txt", "deleted-" + date + "-2.txt",
			"restore-" + date + ".sh", "restore-" + date + "-2.sh",
			"restore-" + date + ".list", "restore-" + date + "-2.list",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o640); err != nil {
				t.Fatalf("test file %s cannot be written: %v", name, err)
			}
		}
		for _, name := range []string{date, date + "-2"} {
			if err := os.Mkdir(filepath.Join(thumbDir, name), 0o750); err != nil {
				t.Fatalf("day directory %s cannot be created: %v", name, err)
			}
		}
	}
	// And this stays, no matter how old: it does not match the shape.
	stayers := []string{
		"deleted-2025-01-04-IMPORTANT.txt",
		"restore-2025-01-04-123.sh", // three digits; maxRunsPerDay is 99
		"restore-2025-01-04-a.sh",
	}
	for _, name := range stayers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o640); err != nil {
			t.Fatalf("test file cannot be written: %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(thumbDir, "2025-01-04-abc"), 0o750); err != nil {
		t.Fatalf("day directory cannot be created: %v", err)
	}

	log, _ := captureLog()
	res, err := CleanArtifacts(log, testJob(dir, thumbDir), now)
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	// Eight scripts, eight lists, eight day directories; of each the newest
	// three stay (guardrail 6), so five go. The eight reports are not yet 365
	// days old and all stay.
	if res.Scripts != 5 || res.Lists != 5 || res.DayDirs != 5 {
		t.Errorf("cleaned: %d scripts, %d lists, %d day directories; want 5/5/5", res.Scripts, res.Lists, res.DayDirs)
	}
	if res.Reports != 0 {
		t.Errorf("%d reports were cleaned; those are not yet 365 days old", res.Reports)
	}
	for _, name := range stayers {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should have stayed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(thumbDir, "2025-01-04-abc")); err != nil {
		t.Errorf("day directory 2025-01-04-abc should have stayed: %v", err)
	}
}

// The dry-run directory sits next to the real artifacts and is never cleaned up
// — it matches no pattern and the cleanup does not descend into it.
func TestCleanArtifactsLeavesTheDryRunDirectory(t *testing.T) {
	dir := t.TempDir()
	thumbDir := t.TempDir()
	now := time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local)

	for _, root := range []string{dir, thumbDir} {
		dryRun := filepath.Join(root, dryRunDir)
		if err := os.Mkdir(dryRun, 0o750); err != nil {
			t.Fatalf("dry-run directory cannot be created: %v", err)
		}
		// Year-old content with names that would match the pattern without the
		// directory around them.
		for _, name := range []string{"deleted.txt", "restore.sh", "restore.list", "deleted-2025-01-01.txt"} {
			if err := os.WriteFile(filepath.Join(dryRun, name), []byte("x"), 0o640); err != nil {
				t.Fatalf("test file cannot be written: %v", err)
			}
		}
	}

	log, _ := captureLog()
	res, err := CleanArtifacts(log, testJob(dir, thumbDir), now)
	if err != nil {
		t.Fatalf("cleanup returned an error: %v", err)
	}
	if res.Total() != 0 {
		t.Errorf("%d items were cleaned; nothing in the dry-run directory should be touched", res.Total())
	}
	for _, root := range []string{dir, thumbDir} {
		items, err := os.ReadDir(filepath.Join(root, dryRunDir))
		if err != nil {
			t.Fatalf("dry-run directory not readable: %v", err)
		}
		if len(items) != 4 {
			t.Errorf("dry-run directory in %s holds %d files, want 4", root, len(items))
		}
	}
}

// The patterns of guardrail 3, on their own. They sit in four places in the
// program against the names ArtifactSlot makes; if those drift apart, artifacts
// either stay forever or something is thrown away that is not ours. This table
// is the place where that shows.
func TestCleanupPatternsAgainstTheNamesOfARun(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 15, 0, 0, time.Local)
	for _, c := range []struct {
		name  string
		stamp string
	}{
		{"first run", "2026-09-01"},
		{"second run", "2026-09-01-2"},
		{"ninety-ninth run", "2026-09-01-99"},
	} {
		t.Run(c.name, func(t *testing.T) {
			slot := ArtifactSlot{Dir: "/var/log/photowatch", DayDir: "/mnt/tank/photowatch/" + c.stamp, Stamp: c.stamp}
			for pattern, name := range map[*regexp.Regexp]string{
				reportName:        slot.ReportName(),
				restoreScriptName: slot.ScriptName(),
				restoreListName:   slot.ListName(),
				dayDirName:        filepath.Base(slot.DayDir),
			} {
				match := pattern.FindStringSubmatch(name)
				if match == nil {
					t.Errorf("%s does not match %s; then it is never cleaned up", name, pattern)
					continue
				}
				if match[1] != now.Format("2006-01-02") {
					t.Errorf("%s yields date %q, want %s", name, match[1], now.Format("2006-01-02"))
				}
			}
		})
	}
}
