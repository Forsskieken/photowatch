package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// quietLog is the logger for tests that only check the file on disk.
// WriteReport and WriteState only log a warning when the group cannot be set;
// that text is not inspected here.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sampleReportData(t *testing.T, count int) ReportData {
	t.Helper()
	var paths []DeletedFile
	for i := 0; i < count; i++ {
		path := filepath.Join("/mnt/tank/photos/2019", fmt.Sprintf("IMG_%04d.JPG", i))
		paths = append(paths, DeletedFile{Raw: path, Path: path, Decodable: true})
	}
	return ReportData{
		Name:             "deleted-2026-08-29.txt",
		Dataset:          "tank/photos",
		PreviousSnapshot: "tank/photos@photowatch-2026-08-28",
		PreviousShort:    "photowatch-2026-08-28",
		PreviousTime:     time.Date(2026, 8, 28, 8, 17, 3, 0, time.Local),
		NewSnapshot:      "tank/photos@photowatch-2026-08-29",
		Now:              time.Date(2026, 8, 29, 8, 16, 12, 0, time.Local),
		Mountpoint:       "/mnt/tank/photos",
		Threshold:        20,
		Alert:            true,
		Diff:             &DiffResult{Deleted: paths, DeletedDirs: 2, Renamed: 4, Added: 12},
	}
}

func TestWriteReportContentAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteReport(quietLog(), dir, sampleReportData(t, 3))
	if err != nil {
		t.Fatalf("writing the report failed: %v", err)
	}
	if filepath.Base(path) != "deleted-2026-08-29.txt" {
		t.Errorf("file name = %s", filepath.Base(path))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("report not found: %v", err)
	}
	if fi.Mode().Perm() != modeReport {
		t.Errorf("permissions = %o, want %o (the group of the directory must be able to read)", fi.Mode().Perm(), modeReport)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report not readable: %v", err)
	}
	text := string(data)
	for _, must := range []string{
		"tank/photos@photowatch-2026-08-28",
		"/mnt/tank/photos/.zfs/snapshot/photowatch-2026-08-28/", // ready-made restore path
		"rsync -aHAX",
		"threshold 20, alert: yes",
		"23 hours 59 minutes",
	} {
		if !strings.Contains(text, must) {
			t.Errorf("the report is missing %q\n---\n%s", must, text)
		}
	}
}

func TestWriteReportTruncatesLongList(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteReport(quietLog(), dir, sampleReportData(t, maxReportLines+7))
	if err != nil {
		t.Fatalf("writing the report failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report not readable: %v", err)
	}
	if !strings.Contains(string(data), "and 7 more lines") {
		t.Error("the closing line about the truncation is missing")
	}
	// +2 for the source and target line of the rsync example at the top, which
	// use the first path from the list.
	if n := strings.Count(string(data), "IMG_"); n != maxReportLines+2 {
		t.Errorf("there are %d path lines in the report, want %d", n, maxReportLines+2)
	}
}

func TestWriteStateIsReadableJSON(t *testing.T) {
	dir := t.TempDir()
	sf := StateFile{
		Written:  time.Now().Format(time.RFC3339),
		Reported: true,
		Payload:  Payload{Version: 1, Status: "ok", Dataset: "tank/photos", Deleted: 7, Examples: []string{"/mnt/tank/photos/x.jpg"}},
	}
	path, err := WriteState(quietLog(), dir, sf)
	if err != nil {
		t.Fatalf("writing the state failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state not readable: %v", err)
	}
	var back StateFile
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("status.json is not valid JSON: %v", err)
	}
	if back.Payload.Deleted != 7 || back.Payload.Dataset != "tank/photos" {
		t.Errorf("state came back mangled: %+v", back)
	}
	if !back.Reported {
		t.Error("reported did not come back from status.json; the OnFailure unit reads exactly that field")
	}

	// ReadState must return the same, because the OnFailure unit leans on it.
	read, found, err := ReadState(dir)
	if err != nil || !found {
		t.Fatalf("ReadState: found=%v, err=%v", found, err)
	}
	if !read.Reported || read.Payload.Deleted != 7 {
		t.Errorf("ReadState gave %+v", read)
	}
	if _, found, err := ReadState(t.TempDir()); err != nil || found {
		t.Errorf("an empty directory must give 'never run yet', not an error: found=%v err=%v", found, err)
	}
	// Nothing of a previous write may stay behind.
	rest, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("directory not readable: %v", err)
	}
	if len(rest) != 1 {
		t.Errorf("there are %d files in the state directory; the temporary file was not cleaned up", len(rest))
	}
}

// The reports and the thumbnails must be openable without sudo, because the
// notification on the phone points at them. A new file gets the group of the
// process (root), not that of the directory, so that is corrected explicitly.
// Which group it is does not matter to the code — this test therefore simply
// uses a second group of the test user.
func TestWriteReportInheritsTheGroupOfTheDirectory(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("groups cannot be queried: %v", err)
	}
	own := os.Getgid()
	other := -1
	for _, g := range groups {
		if g != own {
			other = g
			break
		}
	}
	if other == -1 {
		t.Skip("this user is in only one group; there is nothing to distinguish")
	}

	dir := t.TempDir()
	if err := os.Chown(dir, -1, other); err != nil {
		t.Skipf("group of the test directory cannot be set: %v", err)
	}
	path, err := WriteReport(quietLog(), dir, sampleReportData(t, 2))
	if err != nil {
		t.Fatalf("writing the report failed: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("report not found: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no unix ownership on this system")
	}
	if int(st.Gid) != other {
		t.Errorf("group of the report = %d, want %d (that of the directory); otherwise the report cannot be read", st.Gid, other)
	}
}

// A failed chown (the group of the directory does not exist on the host, or
// somebody runs it by hand with their own REPORT_DIR) may not throw the report
// away: the only damage is that one user cannot read it.
func TestWriteReportStaysWhenTheGroupCannotBeSet(t *testing.T) {
	old := setGroup
	setGroup = func(dir, file string) error {
		return errors.New("chown: operation not permitted")
	}
	t.Cleanup(func() { setGroup = old })

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	dir := t.TempDir()
	path, err := WriteReport(log, dir, sampleReportData(t, 3))
	if err != nil {
		t.Fatalf("the report was thrown away over a failed chown: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the report is not there: %v", err)
	}
	if !strings.Contains(string(data), "IMG_0000.JPG") {
		t.Error("the report is there, but without the list of deleted files")
	}
	if !strings.Contains(buf.String(), "group of the new file cannot be set") {
		t.Errorf("a warning belongs in the log, otherwise nobody notices: %s", buf.String())
	}
	// The state file must not break on this either; the OnFailure unit reads it
	// in a moment.
	if _, err := WriteState(log, t.TempDir(), StateFile{Written: "now"}); err != nil {
		t.Errorf("status.json was thrown away over a failed chown: %v", err)
	}
}

func TestSafeFilePathRejectsEscape(t *testing.T) {
	if _, err := safeFilePath("/var/log/photowatch", "../../etc/passwd"); err == nil {
		t.Error("a name with .. was accepted")
	}
	if _, err := safeFilePath("relative/path", "x.txt"); err == nil {
		t.Error("a relative directory was accepted")
	}
	path, err := safeFilePath("/var/log/photowatch", "deleted-2026-08-29.txt")
	if err != nil {
		t.Fatalf("a valid path was refused: %v", err)
	}
	if path != "/var/log/photowatch/deleted-2026-08-29.txt" {
		t.Errorf("path = %s", path)
	}
}

func TestRelativeWithin(t *testing.T) {
	if rel, ok := relativeWithin("/mnt/tank/photos", "/mnt/tank/photos/2019/x.jpg"); !ok || rel != "2019/x.jpg" {
		t.Errorf("rel = %q, ok = %v", rel, ok)
	}
	if _, ok := relativeWithin("/mnt/tank/photos", "/mnt/tank/photobook/x.jpg"); ok {
		t.Error("a path outside the mountpoint was counted as inside anyway")
	}
}

// The raw form of a path goes through pathText as well. In this branch — the
// round trip failed — the assumption "zfs diff escapes everything below space"
// does not hold: the field is exactly what sat between two tabs. An ESC in the
// report performs cursor movements during `cat`, in the file the reader has to
// decide from whether to restore.
func TestReportFiltersControlCharsFromTheRawForm(t *testing.T) {
	dir := t.TempDir()
	g := sampleReportData(t, 1)
	junk := "/mnt/tank/photos/\x1b[2Jgone\rline\x07.jpg"
	g.Diff.Deleted = append(g.Diff.Deleted, DeletedFile{Raw: junk, Path: junk, Decodable: false})
	g.Diff.NotDecodable = 1
	// The mountpoint and the snapshot name come from outside too (`zfs get`,
	// `zfs list`) and land as text in the same file.
	g.Mountpoint = "/mnt/tank/\x1bphotos"
	g.PreviousSnapshot = "tank/photos@photowatch\x072026-08-28"
	g.PathPrefix = "/mnt/tank/\x1bphotos"

	path, err := WriteReport(quietLog(), dir, g)
	if err != nil {
		t.Fatalf("writing the report failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report not readable: %v", err)
	}
	for _, char := range []string{"\x1b", "\r", "\x07"} {
		if bytes.Contains(data, []byte(char)) {
			t.Errorf("the report holds the control character %q; pathText should have replaced it", char)
		}
	}
	// The rest of the path must still be there, otherwise it has become
	// unrecognizable.
	if !bytes.Contains(data, []byte("gone line")) {
		t.Errorf("the path has become unrecognizable:\n%s", data)
	}
}

// A second run on the same day may not overwrite the restore script, the list,
// the report and the day directory of the first run. Without the stamp in
// ArtifactSlot this test yields the same names twice.
func TestChooseArtifactSlotMovesAsideForAnExistingSet(t *testing.T) {
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	now := time.Date(2026, 9, 1, 8, 15, 0, 0, time.Local)

	first, err := ChooseArtifactSlot(reportDir, thumbDir, now, false)
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	if first.ReportName() != "deleted-2026-09-01.txt" || first.ScriptName() != "restore-2026-09-01.sh" ||
		first.ListName() != "restore-2026-09-01.list" || first.DayDir != filepath.Join(thumbDir, "2026-09-01") {
		t.Fatalf("the first run of a day should get the bare date, got %+v", first)
	}
	if !first.First(now) {
		t.Error("the first set of today does not count as first")
	}

	// Each of the four artifacts must be enough on its own to move aside: if a
	// run strands halfway, only one of the four exists.
	for _, existing := range []string{
		filepath.Join(reportDir, "deleted-2026-09-01.txt"),
		filepath.Join(reportDir, "restore-2026-09-01.sh"),
		filepath.Join(reportDir, "restore-2026-09-01.list"),
	} {
		t.Run(filepath.Base(existing), func(t *testing.T) {
			if err := os.WriteFile(existing, []byte("from the first run"), 0o640); err != nil {
				t.Fatalf("test file cannot be written: %v", err)
			}
			defer os.Remove(existing)
			second, err := ChooseArtifactSlot(reportDir, thumbDir, now, false)
			if err != nil {
				t.Fatalf("second slot: %v", err)
			}
			if second.Stamp != "2026-09-01-2" {
				t.Errorf("stamp %q, want 2026-09-01-2", second.Stamp)
			}
			if second.First(now) {
				t.Error("the second set wrongly counts as the first of today")
			}
		})
	}

	// A day directory alone counts too.
	if err := os.Mkdir(filepath.Join(thumbDir, "2026-09-01"), 0o750); err != nil {
		t.Fatalf("day directory cannot be created: %v", err)
	}
	second, err := ChooseArtifactSlot(reportDir, thumbDir, now, false)
	if err != nil {
		t.Fatalf("second slot: %v", err)
	}
	if second.Stamp != "2026-09-01-2" {
		t.Errorf("an existing day directory alone gives stamp %q, want 2026-09-01-2", second.Stamp)
	}

	// And third, fourth, …
	if err := os.WriteFile(filepath.Join(reportDir, "restore-2026-09-01-2.sh"), []byte("x"), 0o640); err != nil {
		t.Fatalf("test file cannot be written: %v", err)
	}
	third, err := ChooseArtifactSlot(reportDir, thumbDir, now, false)
	if err != nil {
		t.Fatalf("third slot: %v", err)
	}
	if third.Stamp != "2026-09-01-3" {
		t.Errorf("stamp %q, want 2026-09-01-3", third.Stamp)
	}
}

// A dry run writes into a directory of its own and does not touch the real
// artifacts.
func TestChooseArtifactSlotDryRunHasItsOwnDirectory(t *testing.T) {
	reportDir := t.TempDir()
	thumbDir := t.TempDir()
	now := time.Date(2026, 9, 1, 8, 15, 0, 0, time.Local)
	// There is already a real set of today; a dry run may not touch it and does
	// not have to move aside for it either.
	if err := os.WriteFile(filepath.Join(reportDir, "restore-2026-09-01.sh"), []byte("real"), 0o640); err != nil {
		t.Fatalf("test file cannot be written: %v", err)
	}

	slot, err := ChooseArtifactSlot(reportDir, thumbDir, now, true)
	if err != nil {
		t.Fatalf("dry-run slot: %v", err)
	}
	if slot.Dir != filepath.Join(reportDir, "dry-run") {
		t.Errorf("dry run writes in %q, want %q", slot.Dir, filepath.Join(reportDir, "dry-run"))
	}
	if slot.DayDir != filepath.Join(thumbDir, "dry-run") {
		t.Errorf("dry-run day directory %q, want %q", slot.DayDir, filepath.Join(thumbDir, "dry-run"))
	}
	// Fixed names: a dry run only replaces its own previous result, and those
	// names deliberately do not match the cleanup patterns.
	if slot.ReportName() != "deleted.txt" || slot.ScriptName() != "restore.sh" || slot.ListName() != "restore.list" {
		t.Errorf("dry-run names %s/%s/%s", slot.ReportName(), slot.ScriptName(), slot.ListName())
	}
	if reportName.MatchString(slot.ReportName()) || restoreScriptName.MatchString(slot.ScriptName()) ||
		restoreListName.MatchString(slot.ListName()) || dayDirName.MatchString(filepath.Base(slot.DayDir)) {
		t.Error("a name from the dry-run directory matches a cleanup pattern; then the cleanup could remove it one day")
	}
}

// Without a thumbnail directory there is no day directory, and then the choice
// may not break on it either.
func TestChooseArtifactSlotWithoutThumbnailDir(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 15, 0, 0, time.Local)
	slot, err := ChooseArtifactSlot(t.TempDir(), "", now, false)
	if err != nil {
		t.Fatalf("slot without thumbnail directory: %v", err)
	}
	if slot.DayDir != "" {
		t.Errorf("day directory %q, want empty", slot.DayDir)
	}
}

// makeDirWithGroup used to repair only the last directory. If the parent did
// not exist yet, os.MkdirAll created it with the group of the process (root);
// setGroup then inherited that fresh root group from the parent, "succeeded"
// and stayed silent. With `hide unreadable = yes` in Samba you then see nothing
// — no directory and no error.
func TestMakeDirWithGroupHandlesEveryNewLevel(t *testing.T) {
	old := setGroup
	var set []string
	setGroup = func(dir, target string) error {
		set = append(set, target)
		return old(dir, target)
	}
	t.Cleanup(func() { setGroup = old })

	root := t.TempDir()
	middle := filepath.Join(root, "photowatch")
	dayDir := filepath.Join(middle, "2026-09-02")
	if err := makeDirWithGroup(quietLog(), dayDir); err != nil {
		t.Fatalf("directory cannot be created: %v", err)
	}
	for _, path := range []string{middle, dayDir} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s does not exist: %v", path, err)
		}
		if fi.Mode().Perm() != modeDir {
			t.Errorf("mode of %s = %o, want %o", path, fi.Mode().Perm(), modeDir)
		}
		found := false
		for _, g := range set {
			if g == path {
				found = true
			}
		}
		if !found {
			t.Errorf("the group of %s was never set; touched were: %v", path, set)
		}
	}
}

// The flip side: a directory that is already there is not ours. REPORT_DIR is
// set by hand during installation to root:<group> 750, and this function may
// not overwrite that setting with the group of /var/log.
func TestMakeDirWithGroupLeavesAnExistingDirectoryAlone(t *testing.T) {
	old := setGroup
	var set []string
	setGroup = func(dir, target string) error {
		set = append(set, target)
		return old(dir, target)
	}
	t.Cleanup(func() { setGroup = old })

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("mode cannot be set: %v", err)
	}
	if err := makeDirWithGroup(quietLog(), dir); err != nil {
		t.Fatalf("an existing directory returned an error: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory cannot be stat'ed: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode of the existing directory changed to %o", fi.Mode().Perm())
	}
	if len(set) != 0 {
		t.Errorf("the group of an existing directory was touched: %v", set)
	}
}

// Chmod and Chown follow a symlink. If there were a symlink with the name of
// the day directory, they would set the permissions of something entirely
// different; MkdirAll let that pass silently because the symlink points at a
// directory.
func TestMakeDirWithGroupRefusesASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("directory cannot be created: %v", err)
	}
	link := filepath.Join(root, "2026-09-02")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink cannot be created: %v", err)
	}
	if err := makeDirWithGroup(quietLog(), link); err == nil {
		t.Error("a symlink was accepted as a day directory")
	}
	fi, err := os.Stat(real)
	if err != nil {
		t.Fatalf("target of the symlink cannot be stat'ed: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("the permissions of the symlink target changed to %o", fi.Mode().Perm())
	}
}
