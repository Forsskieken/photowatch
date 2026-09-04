package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRestoreInput(t *testing.T, source SnapshotSource, dir string, deleted []DeletedFile) RestoreInput {
	t.Helper()
	return RestoreInput{
		Dir:           dir,
		ScriptName:    "restore-2026-08-31.sh",
		ListName:      "restore-2026-08-31.list",
		Now:           time.Date(2026, 8, 31, 8, 15, 0, 0, time.Local),
		Dataset:       "tank/photos",
		Mountpoint:    source.Mountpoint,
		SnapshotShort: "photowatch-2026-08-30",
		PreviousTime:  time.Date(2026, 8, 30, 8, 17, 3, 0, time.Local),
		RecoverUntil:  time.Date(2026, 9, 13, 8, 17, 3, 0, time.Local),
		SnapshotDir:   source.Dir,
		ZfsPath:       "/usr/sbin/zfs",
		Deleted:       deleted,
		MediaFiles:    len(deleted),
	}
}

// The core of the design: the file names travel NUL separated through the list
// file and never end up in the script. A name with a space, a quote and a $ is
// exactly the case shell text breaks on.
func TestWriteRestoreKeepsNamesOutOfTheScript(t *testing.T) {
	source, _ := testSource(t)
	dir := t.TempDir()
	awkward := putFile(t, source, `From the old box/robin's "holiday" $HOME (5).JPG`, []byte("x"))
	plain := putFile(t, source, "2019/IMG_4412.JPG", []byte("x"))

	log, _ := captureLog()
	res, err := WriteRestore(log, testRestoreInput(t, source, dir, []DeletedFile{awkward, plain}))
	if err != nil {
		t.Fatalf("writing the restore failed: %v", err)
	}
	if res.Lines != 2 {
		t.Fatalf("lines = %d, want 2 (%v, %s)", res.Lines, res.Reasons, res.Message)
	}

	list, err := os.ReadFile(res.ListPath)
	if err != nil {
		t.Fatalf("list file not readable: %v", err)
	}
	parts := bytes.Split(list, []byte{0})
	// After the last NUL there is nothing; that empty tail does not count.
	if len(parts) != 3 || len(parts[2]) != 0 {
		t.Fatalf("list holds %d NUL separated parts: %q", len(parts), list)
	}
	want := map[string]bool{
		`From the old box/robin's "holiday" $HOME (5).JPG`: false,
		"2019/IMG_4412.JPG": false,
	}
	for _, d := range parts[:2] {
		if _, ok := want[string(d)]; !ok {
			t.Errorf("unexpected path in the list: %q", d)
			continue
		}
		want[string(d)] = true
	}
	for path, found := range want {
		if !found {
			t.Errorf("path %q is not in the list unedited", path)
		}
	}

	script, err := os.ReadFile(res.ScriptPath)
	if err != nil {
		t.Fatalf("script not readable: %v", err)
	}
	for _, forbidden := range []string{`robin's`, `"holiday"`, "$HOME", "IMG_4412", "(5)"} {
		if strings.Contains(string(script), forbidden) {
			t.Errorf("the script holds %q; file names belong only in the list file", forbidden)
		}
	}
	// The script can only create, never modify or delete.
	if !strings.Contains(string(script), "--ignore-existing") {
		t.Error("--ignore-existing is missing; the script could then overwrite files")
	}
	// Only look at the executable lines: the comment at the top deliberately
	// explains that no --delete occurs in it.
	for nr, line := range strings.Split(string(script), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "--delete") {
			t.Errorf("line %d of the script holds --delete: %s", nr+1, line)
		}
	}
	if !strings.Contains(string(script), "--from0") || !strings.Contains(string(script), "--files-from=") {
		t.Error("the script does not read the names NUL separated from the list file")
	}
}

func TestWriteRestorePermissionsAndSyntax(t *testing.T) {
	source, _ := testSource(t)
	dir := t.TempDir()
	photo := putFile(t, source, "2019/IMG_4412.JPG", []byte("x"))
	log, _ := captureLog()
	res, err := WriteRestore(log, testRestoreInput(t, source, dir, []DeletedFile{photo}))
	if err != nil {
		t.Fatalf("writing the restore failed: %v", err)
	}

	fi, err := os.Stat(res.ScriptPath)
	if err != nil {
		t.Fatalf("script not found: %v", err)
	}
	if fi.Mode().Perm() != modeScript {
		t.Errorf("permissions of the script = %o, want %o", fi.Mode().Perm(), modeScript)
	}
	fi, err = os.Stat(res.ListPath)
	if err != nil {
		t.Fatalf("list not found: %v", err)
	}
	if fi.Mode().Perm() != modeList {
		t.Errorf("permissions of the list = %o, want %o", fi.Mode().Perm(), modeList)
	}
	if filepath.Base(res.ScriptPath) != "restore-2026-08-31.sh" {
		t.Errorf("script name = %s", filepath.Base(res.ScriptPath))
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine; the syntax check is skipped")
	}
	// bash -n executes nothing but does check the whole syntax. Without this
	// check a typo in the generated script would only show up the moment
	// somebody needs it in a panic.
	out, err := exec.Command(bash, "-n", res.ScriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("the generated script is not valid bash: %v\n%s", err, out)
	}
}

// Without root the script does nothing, not even with --apply. That is the
// first of the five checks it starts with.
func TestRestoreScriptRefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test does not run as root")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine")
	}
	source, _ := testSource(t)
	dir := t.TempDir()
	photo := putFile(t, source, "2019/IMG_4412.JPG", []byte("x"))
	log, _ := captureLog()
	res, err := WriteRestore(log, testRestoreInput(t, source, dir, []DeletedFile{photo}))
	if err != nil {
		t.Fatalf("writing the restore failed: %v", err)
	}
	out, err := exec.Command(bash, res.ScriptPath, "--apply").CombinedOutput()
	if err == nil {
		t.Fatalf("the script ran on without root:\n%s", out)
	}
	if !strings.Contains(string(out), "as root") {
		t.Errorf("the message does not explain that root is needed:\n%s", out)
	}
}

// The second safeguard on generating shell text: if one value does not match
// ^[A-Za-z0-9._/-]+$, there is *no* script.
func TestWriteRestoreRejectsSuspectValues(t *testing.T) {
	source, _ := testSource(t)
	cases := []struct {
		name    string
		adjust  func(g *RestoreInput)
		lookFor string
	}{
		{"mountpoint with a quote", func(g *RestoreInput) { g.Mountpoint = `/mnt/tank/pho"tos` }, "mountpoint"},
		{"dataset with a semicolon", func(g *RestoreInput) { g.Dataset = "tank/photos;rm -rf /" }, "dataset name"},
		{"snapshot name with a space", func(g *RestoreInput) { g.SnapshotShort = "photowatch 2026-08-30" }, "snapshot name"},
		{"snapshot dir with a $", func(g *RestoreInput) { g.SnapshotDir = "/mnt/$HOME/.zfs" }, "snapshot directory"},
		{"zfs path with a backtick", func(g *RestoreInput) { g.ZfsPath = "/usr/sbin/`id`" }, "zfs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			photo := putFile(t, source, "2019/IMG_4412.JPG", []byte("x"))
			g := testRestoreInput(t, source, dir, []DeletedFile{photo})
			c.adjust(&g)
			log, buf := captureLog()
			res, err := WriteRestore(log, g)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.ScriptPath != "" {
				t.Errorf("a script was written anyway: %s", res.ScriptPath)
			}
			if !strings.Contains(res.Message, c.lookFor) {
				t.Errorf("the message does not name the offending value: %q", res.Message)
			}
			items, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("directory not readable: %v", err)
			}
			if len(items) != 0 {
				t.Errorf("there are files in %s after all: %v", dir, items)
			}
			if !strings.Contains(buf.String(), "did not pass validation") {
				t.Errorf("the log does not explain why there is no script: %s", buf.String())
			}
		})
	}
}

// The report explains why there is no script and then gives the manual rsync
// example — otherwise you are left empty-handed.
func TestReportExplainsWhyThereIsNoScript(t *testing.T) {
	dir := t.TempDir()
	g := sampleReportData(t, 2)
	g.Restore = RestoreResult{Message: "no restore script written: the mountpoint of the dataset holds odd characters"}
	path, err := WriteReport(quietLog(), dir, g)
	if err != nil {
		t.Fatalf("writing the report failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report not readable: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "the mountpoint of the dataset holds odd characters") {
		t.Errorf("the report does not explain why there is no script:\n%s", text)
	}
	if !strings.Contains(text, "rsync -aHAX") {
		t.Errorf("the manual example is missing:\n%s", text)
	}
}

// Only what is really in the snapshot goes into the list: a path that cannot be
// decoded, a path outside the mountpoint and a file that is no longer there all
// three drop out — with a reason.
func TestWriteRestoreSkipsUnusablePaths(t *testing.T) {
	source, _ := testSource(t)
	dir := t.TempDir()
	good := putFile(t, source, "2019/IMG_4412.JPG", []byte("x"))
	broken := DeletedFile{Raw: `/x/broken\012path.jpg`, Path: `/x/broken\012path.jpg`, Decodable: false}
	outside := DeletedFile{Raw: "/elsewhere/a.jpg", Path: "/elsewhere/a.jpg", Decodable: true}
	gone := DeletedFile{Raw: "/x/gone.jpg", Path: filepath.Join(source.Mountpoint, "gone.jpg"), Decodable: true}

	log, _ := captureLog()
	res, err := WriteRestore(log, testRestoreInput(t, source, dir, []DeletedFile{good, broken, outside, gone}))
	if err != nil {
		t.Fatalf("writing the restore failed: %v", err)
	}
	if res.Lines != 1 {
		t.Errorf("lines = %d, want 1 (%v)", res.Lines, res.Reasons)
	}
	for reason, want := range map[string]int{
		"path not decodable":                1,
		"path falls outside the mountpoint": 1,
		"not present in the snapshot":       1,
	} {
		if res.Reasons[reason] != want {
			t.Errorf("reason %q = %d, want %d (all: %v)", reason, res.Reasons[reason], want, res.Reasons)
		}
	}
}

// When nothing is left in the snapshot, there is no half script but a message
// saying what is going on.
func TestWriteRestoreWithoutUsablePaths(t *testing.T) {
	source, _ := testSource(t)
	dir := t.TempDir()
	gone := DeletedFile{Raw: "/x/gone.jpg", Path: filepath.Join(source.Mountpoint, "gone.jpg"), Decodable: true}
	log, buf := captureLog()
	res, err := WriteRestore(log, testRestoreInput(t, source, dir, []DeletedFile{gone}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ScriptPath != "" || res.ListPath != "" {
		t.Errorf("something was written anyway: %s %s", res.ScriptPath, res.ListPath)
	}
	if res.Message == "" {
		t.Error("there is no message; then it stays silent")
	}
	if !strings.Contains(buf.String(), "no path passed the checks") {
		t.Errorf("the log does not say why there is no script: %s", buf.String())
	}
}

// *Every* value interpolated into the script belongs in the validation.
// scriptPath was missing from it; that went unnoticed because scriptPath and
// listPath come from the same directory and therefore always pass or fail
// together. This test checks the property itself and not that coincidence.
func TestScriptValuesCoversEverythingInTheScript(t *testing.T) {
	source, _ := testSource(t)
	g := testRestoreInput(t, source, t.TempDir(), nil)
	scriptPath := filepath.Join(g.Dir, "restore-2026-08-31.sh")
	listPath := filepath.Join(g.Dir, "restore-2026-08-31.list")

	script := buildRestoreScript(g, scriptPath, listPath, 1)
	validated := map[string]bool{}
	for _, value := range scriptValues(g, scriptPath, listPath) {
		validated[value] = true
	}

	for _, value := range []string{
		g.Mountpoint, g.Dataset, g.SnapshotShort, g.SnapshotDir, g.ZfsPath, scriptPath, listPath,
	} {
		if !strings.Contains(script, value) {
			t.Fatalf("the test is no longer sound: %q is not in the script", value)
		}
		if !validated[value] {
			t.Errorf("%q ends up in the script but is not validated against %s",
				value, scriptValueRegexp.String())
		}
	}
}

// WriteRestore runs before WriteReport, and the latter does the MkdirAll. If
// the directory is missing (install -d skipped), the restore script was exactly
// what fell away on the first run with deletions — the run where the script is
// needed most.
func TestWriteRestoreCreatesTheDirectory(t *testing.T) {
	source, _ := testSource(t)
	dir := filepath.Join(t.TempDir(), "photowatch") // does not exist yet
	photo := putFile(t, source, "2019/IMG_4412.JPG", []byte("x"))

	log, _ := captureLog()
	res, err := WriteRestore(log, testRestoreInput(t, source, dir, []DeletedFile{photo}))
	if err != nil {
		t.Fatalf("writing the restore failed in a directory that did not exist yet: %v", err)
	}
	if res.ScriptPath == "" || res.ListPath == "" {
		t.Fatalf("no script or list was written: %q %q (%s)", res.ScriptPath, res.ListPath, res.Message)
	}
	if _, err := os.Stat(res.ScriptPath); err != nil {
		t.Errorf("the script is not there: %v", err)
	}
}

// --files-from implies --relative, and without --no-implied-dirs, --relative
// carries over the owner, permissions and mtime of every intermediate directory
// from the snapshot. A restore would then roll back the permissions of an
// existing directory by fourteen days, while the documentation promises that
// the script can only create.
func TestRestoreScriptLeavesExistingDirectoriesAlone(t *testing.T) {
	source, _ := testSource(t)
	dir := t.TempDir()
	photo := putFile(t, source, "From the old box/IMG_4412.JPG", []byte("x"))
	log, _ := captureLog()
	res, err := WriteRestore(log, testRestoreInput(t, source, dir, []DeletedFile{photo}))
	if err != nil {
		t.Fatalf("writing the restore failed: %v", err)
	}
	script, err := os.ReadFile(res.ScriptPath)
	if err != nil {
		t.Fatalf("script not readable: %v", err)
	}
	for _, flag := range []string{"--no-implied-dirs", "--omit-dir-times"} {
		if !strings.Contains(string(script), flag) {
			t.Errorf("%s is missing; a restore would then change the attributes of existing directories", flag)
		}
	}
}
