package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fixed sample output of `zfs diff -H -F`. The fields are tab separated. Note
// the paths: ZFS escapes everything below space, the space itself, the
// backslash and every non-ASCII byte octally. A space therefore appears as
// \0040 and the é of café as \0303\0251 — quotes and apostrophes do not, those
// it prints literally.
//
// The second to last line is broken on purpose: \012 has three digits instead
// of four, so that path cannot be decoded and must stay in its raw form.
const diffSample = "-\tF\t/mnt/tank/photos/2019/Croatia/IMG_4412.JPG\n" +
	"-\tF\t/mnt/tank/photos/2019/Croatia/IMG_4413.JPG\n" +
	"+\tF\t/mnt/tank/photos/2026/new.jpg\n" +
	"-\tF\t/mnt/tank/photos/2020/robin's\\0040\"holiday\".jpg\n" +
	"-\t/\t/mnt/tank/photos/2018/Empty\n" +
	"-\t@\t/mnt/tank/photos/2018/link\n" +
	"M\tF\t/mnt/tank/photos/2021/modified.jpg\n" +
	"R\tF\t/mnt/tank/photos/2022/old.jpg\t/mnt/tank/photos/2022/new.jpg\n" +
	"-\tF\t/mnt/tank/photos/2017/odd\\0040name.jpg\n" +
	"-\tF\t/mnt/tank/photos/2015/caf\\0303\\0251.jpg\n" +
	"-\tF\t/mnt/tank/photos/2014/broken\\012x.jpg\n" +
	"-\tF\t/mnt/tank/other/outside-the-prefix.jpg\n"

func TestParseDiffWithoutPathPrefix(t *testing.T) {
	res, err := parseDiff(strings.NewReader(diffSample), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := len(res.Deleted), 7; got != want {
		t.Errorf("deleted files = %d, want %d (%v)", got, want, res.Deleted)
	}
	if res.NotDecodable != 1 {
		t.Errorf("not decodable = %d, want 1", res.NotDecodable)
	}
	if res.MediaFiles() != 7 || res.SidecarFiles() != 0 {
		t.Errorf("media/sidecar files = %d/%d, want 7/0", res.MediaFiles(), res.SidecarFiles())
	}
	if res.DeletedDirs != 1 {
		t.Errorf("deleted dirs = %d, want 1", res.DeletedDirs)
	}
	if res.DeletedOther != 1 {
		t.Errorf("deleted other (symlink) = %d, want 1", res.DeletedOther)
	}
	if res.Renamed != 1 {
		t.Errorf("renamed = %d, want 1", res.Renamed)
	}
	if res.Added != 1 {
		t.Errorf("added = %d, want 1", res.Added)
	}
	if res.Modified != 1 {
		t.Errorf("modified = %d, want 1", res.Modified)
	}
	if res.Unparsed != 0 {
		t.Errorf("unparsed = %d, want 0", res.Unparsed)
	}
}

// Every path comes back in two forms: the raw form as zfs gave it, and the
// decoded form that passed the round-trip check. A path that cannot be decoded
// still counts — a file is gone — but does not carry the flag, so that it does
// not end up in the restore script.
func TestParseDiffCarriesBothForms(t *testing.T) {
	res, err := parseDiff(strings.NewReader(diffSample), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]struct {
		path      string
		decodable bool
	}{
		`/mnt/tank/photos/2020/robin's\0040"holiday".jpg`: {`/mnt/tank/photos/2020/robin's "holiday".jpg`, true},
		`/mnt/tank/photos/2017/odd\0040name.jpg`:          {"/mnt/tank/photos/2017/odd name.jpg", true},
		`/mnt/tank/photos/2015/caf\0303\0251.jpg`:         {"/mnt/tank/photos/2015/café.jpg", true},
		// Three octal digits instead of four: not decodable, so Path stays equal
		// to Raw.
		`/mnt/tank/photos/2014/broken\012x.jpg`: {`/mnt/tank/photos/2014/broken\012x.jpg`, false},
	}
	seen := map[string]bool{}
	for _, v := range res.Deleted {
		w, ok := want[v.Raw]
		if !ok {
			continue
		}
		seen[v.Raw] = true
		if v.Path != w.path {
			t.Errorf("path of %q = %q, want %q", v.Raw, v.Path, w.path)
		}
		if v.Decodable != w.decodable {
			t.Errorf("decodable of %q = %v, want %v", v.Raw, v.Decodable, w.decodable)
		}
	}
	for raw := range want {
		if !seen[raw] {
			t.Errorf("path %q did not come back; got: %v", raw, res.Deleted)
		}
	}
}

// The list must be sorted: `zfs diff` gives no guaranteed order, and without
// sorting the files of one directory are spread through the whole report and
// the even sample for the thumbnails is not a cross-section.
func TestParseDiffSorts(t *testing.T) {
	input := "-\tF\t/mnt/tank/photos/c.jpg\n" +
		"-\tF\t/mnt/tank/photos/a.jpg\n" +
		"-\tF\t/mnt/tank/photos/b.jpg\n"
	res, err := parseDiff(strings.NewReader(input), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var paths []string
	for _, v := range res.Deleted {
		paths = append(paths, v.Path)
	}
	want := []string{"/mnt/tank/photos/a.jpg", "/mnt/tank/photos/b.jpg", "/mnt/tank/photos/c.jpg"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", paths, want)
	}
}

// Filtering on PATH_PREFIX works on the decoded path. On the raw form a prefix
// with a space in it would never have matched, because there the space is
// \0040.
func TestParseDiffFiltersOnTheDecodedPath(t *testing.T) {
	input := "-\tF\t/mnt/tank/photos/From\\0040the\\0040old\\0040box/a.jpg\n" +
		"-\tF\t/mnt/tank/photos/Other/b.jpg\n"
	res, err := parseDiff(strings.NewReader(input), "/mnt/tank/photos/From the old box")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Path != "/mnt/tank/photos/From the old box/a.jpg" {
		t.Errorf("deleted = %v, want only /mnt/tank/photos/From the old box/a.jpg", res.Deleted)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", res.Skipped)
	}
}

func TestSidecarFiles(t *testing.T) {
	input := "-\tF\t/mnt/tank/photos/a.jpg\n" +
		"-\tF\t/mnt/tank/photos/a.jpg.xmp\n" +
		"-\tF\t/mnt/tank/photos/folder.nfo\n" +
		"-\tF\t/mnt/tank/photos/Thumbs.db\n"
	res, err := parseDiff(strings.NewReader(input), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.SidecarFiles() != 3 {
		t.Errorf("sidecar files = %d, want 3", res.SidecarFiles())
	}
	if res.MediaFiles() != 1 {
		t.Errorf("media files = %d, want 1", res.MediaFiles())
	}
}

func TestParseDiffWithPathPrefix(t *testing.T) {
	res, err := parseDiff(strings.NewReader(diffSample), "/mnt/tank/photos")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := len(res.Deleted), 6; got != want {
		t.Errorf("deleted within the prefix = %d, want %d (%v)", got, want, res.Deleted)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped outside the prefix = %d, want 1", res.Skipped)
	}
	for _, v := range res.Deleted {
		if strings.HasPrefix(v.Path, "/mnt/tank/other") {
			t.Errorf("path outside the prefix counted anyway: %q", v.Path)
		}
	}
}

func TestWithinPathPrefixNoHalfFolderName(t *testing.T) {
	// /mnt/tank/photobook starts with /mnt/tank/photo but does not belong to it.
	if withinPathPrefix("/mnt/tank/photo", "/mnt/tank/photobook/x.jpg") {
		t.Error("/mnt/tank/photobook/x.jpg was counted inside /mnt/tank/photo")
	}
	if !withinPathPrefix("/mnt/tank/photo", "/mnt/tank/photo/x.jpg") {
		t.Error("/mnt/tank/photo/x.jpg fell outside /mnt/tank/photo")
	}
	if !withinPathPrefix("", "/whatever/it/is") {
		t.Error("without a prefix everything must count")
	}
}

func TestParseDiffUnparsedLines(t *testing.T) {
	input := "nonsense without tabs\n" +
		"\n" +
		"?\tF\t/mnt/tank/photos/x.jpg\n" +
		"-\tF\t/mnt/tank/photos/good.jpg\n"
	res, err := parseDiff(strings.NewReader(input), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Deleted) != 1 {
		t.Errorf("deleted = %d, want 1", len(res.Deleted))
	}
	if res.Unparsed != 2 {
		t.Errorf("unparsed = %d, want 2 (the nonsense line and the unknown mark)", res.Unparsed)
	}
}

func TestParseSnapshotList(t *testing.T) {
	// Output of: zfs list -H -p -o name,creation -t snapshot -r tank/photos
	input := "tank/photos@photowatch-2026-08-27\t1756276623\n" +
		"tank/photos@photowatch-2026-08-28\t1756363023\n" +
		"tank/photos@manual-before-cleanup\t1756363999\n" +
		"tank/photos/test@photowatch-2026-08-28\t1756363050\n" + // child dataset: does not belong here
		"tank/photos@broken\tnot-a-number\n"
	snaps, unparsed, err := parseSnapshotList(strings.NewReader(input), "tank/photos")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("number of snapshots = %d, want 3: %v", len(snaps), snaps)
	}
	// The line without a valid time counts as unparsed; the one of the child
	// dataset does not, because that is normal output of -r.
	if unparsed != 1 {
		t.Errorf("unparsed = %d, want 1 (the line with 'not-a-number'); unparsed lines may not disappear silently", unparsed)
	}
	for _, s := range snaps {
		if strings.HasPrefix(s.Name, "tank/photos/test") {
			t.Errorf("snapshot of a child dataset counted: %s", s.Name)
		}
	}
	if !snaps[1].Created.Equal(time.Unix(1756363023, 0)) {
		t.Errorf("creation time parsed wrongly: %v", snaps[1].Created)
	}
}

func TestLatestWithPrefix(t *testing.T) {
	snaps := []Snapshot{
		{Name: "tank/photos@photowatch-2026-08-27", Short: "photowatch-2026-08-27", Created: time.Unix(100, 0)},
		{Name: "tank/photos@photowatch-2026-08-28", Short: "photowatch-2026-08-28", Created: time.Unix(200, 0)},
		{Name: "tank/photos@something-else", Short: "something-else", Created: time.Unix(300, 0)},
	}
	s, ok := LatestWithPrefix(snaps, "photowatch")
	if !ok {
		t.Fatal("no snapshot found while there are two with the prefix")
	}
	if s.Short != "photowatch-2026-08-28" {
		t.Errorf("newest = %s, want photowatch-2026-08-28", s.Short)
	}
	if _, ok := LatestWithPrefix(snaps, "nonexistent"); ok {
		t.Error("a snapshot was found with a prefix that does not occur")
	}
}

func TestCheckSnapshotNameRejectsDangerousNames(t *testing.T) {
	cases := map[string]bool{ // name -> must be refused
		"tank/photos@photowatch-2026-08-29": false,
		"tank/photos":                       true, // without @ destroy would hit the dataset
		"tank/photos@a%b":                   true, // % is the range separator of zfs destroy
		"tank/photos@a,b":                   true,
		"tank/photos@a b":                   true,
		"-tank/photos@a":                    true,
		"tank/photos@":                      true,
		"@photowatch-2026-08-29":            true,
		"tank/photos@a@b":                   true,
	}
	for name, mustFail := range cases {
		err := checkSnapshotName(name)
		if mustFail && err == nil {
			t.Errorf("name %q was accepted but should have been refused", name)
		}
		if !mustFail && err != nil {
			t.Errorf("name %q was refused: %v", name, err)
		}
	}
}

// A read error halfway may not leave the run hanging. Before the fix,
// cmd.Wait() kept waiting for a zfs that was still writing to a pipe nobody
// drained, and that lasted until the ten-minute timeout — exactly on the
// morning something is wrong.
//
// The fake zfs below first writes one line longer than the scanner's buffer
// (that is the read error) and then keeps going forever.
func TestDiffDoesNotHangAfterAReadError(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh to run the fake zfs with: %v", err)
	}
	fake := filepath.Join(t.TempDir(), "fake-zfs")
	script := "#!/bin/sh\n" +
		"head -c " + strconv.Itoa(maxDiffLine+1000) + " /dev/zero | tr '\\0' 'x'\n" +
		"echo\n" +
		// An M line and not a - line: `yes` would read an argument starting with
		// - as an option and stop right away, and then the fake zfs would
		// precisely not keep writing forever.
		"yes 'M\tF\t/mnt/tank/photos/x.jpg'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("could not write the fake zfs: %v", err)
	}

	z := Zfs{Path: fake}
	done := make(chan error, 1)
	go func() {
		_, err := z.Diff(context.Background(), "tank/photos@photowatch-2026-08-28", "tank/photos", "")
		done <- err
	}()

	// Plenty for a few thousand lines, far below the ten minutes of diffTimeout:
	// if it hangs, that is visible here.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an over-long line produced no error")
		}
		if !strings.Contains(err.Error(), "zfs diff not readable") {
			t.Errorf("the error does not name the real cause (the read error): %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Diff hangs after a read error; it should close the pipe and not wait for zfs")
	}
}
