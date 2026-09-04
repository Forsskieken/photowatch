package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testSource puts down an imitation snapshot directory:
// <mountpoint>/.zfs/snapshot/<name>/, exactly as ZFS offers it.
func testSource(t *testing.T) (SnapshotSource, string) {
	t.Helper()
	root := t.TempDir()
	mountpoint := filepath.Join(root, "archive")
	snapDir := filepath.Join(mountpoint, ".zfs", "snapshot", "photowatch-2026-08-30")
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		t.Fatalf("snapshot directory cannot be created: %v", err)
	}
	target := filepath.Join(t.TempDir(), "2026-08-31")
	return SnapshotSource{Mountpoint: mountpoint, Dir: snapDir}, target
}

// putImage writes a real image into the snapshot directory and returns the
// matching DeletedFile, with the path as it was in the dataset.
func putImage(t *testing.T, source SnapshotSource, rel string, width, height int, format string) DeletedFile {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// A gradient with a block in it: at 320 pixels you must still see the
			// block, and that is exactly what a thumbnail has to do.
			c := color.RGBA{R: uint8(x * 255 / width), G: uint8(y * 255 / height), B: 40, A: 255}
			if x > width/3 && x < width*2/3 && y > height/3 && y < height*2/3 {
				c = color.RGBA{R: 250, G: 250, B: 250, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	switch format {
	case "png":
		if err := png.Encode(&buf, img); err != nil {
			t.Fatalf("png cannot be made: %v", err)
		}
	default:
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatalf("jpeg cannot be made: %v", err)
		}
	}
	return putFile(t, source, rel, buf.Bytes())
}

func putFile(t *testing.T, source SnapshotSource, rel string, content []byte) DeletedFile {
	t.Helper()
	path := filepath.Join(source.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("directory cannot be created: %v", err)
	}
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatalf("file %s cannot be written: %v", rel, err)
	}
	inDataset := filepath.Join(source.Mountpoint, rel)
	return DeletedFile{Raw: EncodeZfsPath([]byte(inDataset)), Path: inDataset, Decodable: true}
}

// fakeLargePNG makes a PNG header promising 40000 by 40000 pixels. There is not
// a single pixel behind it: this is exactly the file DecodeConfig exists for,
// because Decode would reserve 6.4 GB for it.
func fakeLargePNG(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], width)
	binary.BigEndian.PutUint32(ihdr[4:], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // truecolor
	var chunk bytes.Buffer
	chunk.WriteString("IHDR")
	chunk.Write(ihdr)
	if err := binary.Write(&b, binary.BigEndian, uint32(len(ihdr))); err != nil {
		t.Fatalf("length cannot be written: %v", err)
	}
	b.Write(chunk.Bytes())
	if err := binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk.Bytes())); err != nil {
		t.Fatalf("crc cannot be written: %v", err)
	}
	return b.Bytes()
}

func TestMakeThumbnails(t *testing.T) {
	source, target := testSource(t)
	log, _ := captureLog()

	good := putImage(t, source, "2019/Croatia/IMG_4412.JPG", 640, 480, "jpeg")
	alsoGood := putImage(t, source, "From the old box/PICT0033 (5).png", 200, 400, "png")
	notAPhoto := putFile(t, source, "2020/broken.jpg", []byte("this is not a photo but text"))
	tooLarge := putFile(t, source, "2020/huge.png", fakeLargePNG(t, 40000, 40000))
	video := DeletedFile{Raw: "/x/movie.mov", Path: filepath.Join(source.Mountpoint, "movie.mov"), Decodable: true}
	sidecar := DeletedFile{Raw: "/x/a.xmp", Path: filepath.Join(source.Mountpoint, "2019/Croatia/IMG_4412.JPG.xmp"), Decodable: true}
	unparsed := DeletedFile{Raw: `/x/broken\012path.jpg`, Path: `/x/broken\012path.jpg`, Decodable: false}
	gone := DeletedFile{Raw: "/x/vanished.jpg", Path: filepath.Join(source.Mountpoint, "vanished.jpg"), Decodable: true}

	// A symlink with a photo name: it must not be followed.
	symPath := filepath.Join(source.Dir, "link.jpg")
	if err := os.Symlink(filepath.Join(source.Dir, "2019/Croatia/IMG_4412.JPG"), symPath); err != nil {
		t.Fatalf("symlink cannot be created: %v", err)
	}
	symlink := DeletedFile{Raw: "/x/link.jpg", Path: filepath.Join(source.Mountpoint, "link.jpg"), Decodable: true}

	candidates := []DeletedFile{good, alsoGood, notAPhoto, tooLarge, video, sidecar, unparsed, gone, symlink}
	res, err := MakeThumbnails(context.Background(), log, source, target, candidates, ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512})
	if err != nil {
		t.Fatalf("making thumbnails returned an error: %v", err)
	}

	if res.Made != 2 {
		t.Errorf("made = %d, want 2 (%v)", res.Made, res.Reasons)
	}
	if res.Candidates != 6 {
		t.Errorf("candidates = %d, want 6 (jpg/png/gif that are no sidecar and are decodable)", res.Candidates)
	}
	for reason, want := range map[string]int{
		"path not decodable":                 1,
		"sidecar file, not a photo":          1,
		"not jpg/png/gif":                    1,
		"not a readable image":               1,
		"needs too much memory":              1,
		"not (or no longer) in the snapshot": 1,
		"not a regular file in the snapshot": 1,
	} {
		if res.Reasons[reason] != want {
			t.Errorf("reason %q = %d, want %d (all reasons: %v)", reason, res.Reasons[reason], want, res.Reasons)
		}
	}

	items, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("day directory not readable: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("there are %d files in the day directory, want 2", len(items))
	}
	names := []string{items[0].Name(), items[1].Name()}
	// The list is walked in the given order: first the JPEG, then the PNG. The
	// name is cleaned down to [A-Za-z0-9._-].
	want := []string{"001-IMG_4412.jpg", "002-PICT0033__5_.jpg"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("file name %d = %q, want %q", i, names[i], want[i])
		}
		if !thumbnailName.MatchString(names[i]) {
			t.Errorf("name %q does not match the pattern the cleanup knows", names[i])
		}
	}
	if res.Numbers[good.Raw] != 1 || res.Numbers[alsoGood.Raw] != 2 {
		t.Errorf("numbers = %v, want 1 and 2 for the two photos", res.Numbers)
	}

	fi, err := os.Stat(filepath.Join(target, "001-IMG_4412.jpg"))
	if err != nil {
		t.Fatalf("thumbnail not found: %v", err)
	}
	if fi.Mode().Perm() != modeReport {
		t.Errorf("permissions = %o, want %o", fi.Mode().Perm(), modeReport)
	}

	// Is the scale right? 640x480 with long side 320 becomes 320x240.
	f, err := os.Open(filepath.Join(target, "001-IMG_4412.jpg"))
	if err != nil {
		t.Fatalf("thumbnail cannot be opened: %v", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("thumbnail not readable: %v", err)
	}
	if cfg.Width != 320 || cfg.Height != 240 {
		t.Errorf("thumbnail is %dx%d, want 320x240", cfg.Width, cfg.Height)
	}
}

// A second run on the same day may not leave a number behind that points at a
// different photo than the report says.
func TestMakeThumbnailsCleansThePreviousRound(t *testing.T) {
	source, target := testSource(t)
	log, _ := captureLog()
	first := putImage(t, source, "a.jpg", 60, 40, "jpeg")
	second := putImage(t, source, "b.jpg", 60, 40, "jpeg")

	if _, err := MakeThumbnails(context.Background(), log, source, target, []DeletedFile{first, second}, ThumbnailOptions{Max: 24, Px: 320}); err != nil {
		t.Fatalf("first round failed: %v", err)
	}
	// Second round with only the second file: number 001 must now point at b and
	// 002-b.jpg may no longer be there.
	res, err := MakeThumbnails(context.Background(), log, source, target, []DeletedFile{second}, ThumbnailOptions{Max: 24, Px: 320})
	if err != nil {
		t.Fatalf("second round failed: %v", err)
	}
	if res.Made != 1 {
		t.Fatalf("made = %d, want 1", res.Made)
	}
	items, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("day directory not readable: %v", err)
	}
	if len(items) != 1 || items[0].Name() != "001-b.jpg" {
		var names []string
		for _, i := range items {
			names = append(names, i.Name())
		}
		t.Errorf("day directory holds %v, want only 001-b.jpg", names)
	}
}

func TestMakeThumbnailsStopsAboveTheLimit(t *testing.T) {
	source, target := testSource(t)
	log, buf := captureLog()
	photo := putImage(t, source, "a.jpg", 60, 40, "jpeg")
	// Make the root of the thumbnail directory look full with one large file.
	if err := os.WriteFile(filepath.Join(filepath.Dir(target), "filler"), make([]byte, 3<<20), 0o640); err != nil {
		t.Fatalf("filler cannot be written: %v", err)
	}
	res, err := MakeThumbnails(context.Background(), log, source, target, []DeletedFile{photo}, ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Made != 0 {
		t.Errorf("%d thumbnails were made while the directory is over the limit", res.Made)
	}
	if res.Message == "" {
		t.Error("there is no message in the result; then nobody sees why there are no images")
	}
	if !strings.Contains(buf.String(), "too large") {
		t.Errorf("there is no warning in the log: %s", buf.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the day directory was created anyway (err=%v)", err)
	}
}

func TestPathInSnapshotStaysInsideTheSnapshot(t *testing.T) {
	source := SnapshotSource{Mountpoint: "/mnt/tank/photos", Dir: "/mnt/tank/photos/.zfs/snapshot/photowatch-2026-08-30"}
	good, err := pathInSnapshot(source, "/mnt/tank/photos/2019/a.jpg")
	if err != nil {
		t.Fatalf("an ordinary path was refused: %v", err)
	}
	if good != "/mnt/tank/photos/.zfs/snapshot/photowatch-2026-08-30/2019/a.jpg" {
		t.Errorf("path = %q", good)
	}
	for _, bad := range []string{
		"/mnt/tank/photos/../../etc/shadow",
		"/etc/shadow",
		"/mnt/tank/photobook/a.jpg",
		"/mnt/tank/photos",
	} {
		if path, err := pathInSnapshot(source, bad); err == nil {
			t.Errorf("path %q was accepted as %q", bad, path)
		}
	}
}

func TestCleanBaseName(t *testing.T) {
	cases := map[string]string{
		"/a/PICT0033 (5).JPG":               "PICT0033__5_",
		"/a/café.jpg":                       "caf__", // two bytes of the é
		"/a/../../etc/passwd":               "passwd",
		"/a/.hidden.jpg":                    "hidden",
		"/a/" + strings.Repeat("x", 200):    strings.Repeat("x", maxBaseName),
		"/a/....":                           "without-name",
		"/a/name with 'quote' and $var.jpg": "name_with__quote__and__var",
	}
	for in, want := range cases {
		if got := cleanBaseName(in); got != want {
			t.Errorf("cleanBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEvenSample(t *testing.T) {
	var list []DeletedFile
	for i := 0; i < 100; i++ {
		list = append(list, DeletedFile{Raw: string(rune('a' + i%26))})
	}
	if n := len(evenSample(list, 24)); n != 24 {
		t.Errorf("evenSample of 100 with max 24 = %d", n)
	}
	if n := len(evenSample(list[:10], 24)); n != 10 {
		t.Errorf("evenSample of 10 with max 24 = %d, want 10", n)
	}
	if n := len(evenSample(list, 0)); n != 0 {
		t.Errorf("evenSample with max 0 = %d, want 0", n)
	}
}

func TestBoxScaleKeepsTheRatio(t *testing.T) {
	cases := []struct{ bw, bh, wantW, wantH int }{
		{640, 480, 320, 240},
		{480, 640, 240, 320},
		{100, 100, 100, 100}, // smaller than the long side: do not enlarge
		{3000, 10, 320, 1},   // extreme width: height stays at least 1
	}
	for _, c := range cases {
		src := image.NewRGBA(image.Rect(0, 0, c.bw, c.bh))
		dst := boxScale(src, 320)
		if dst.Bounds().Dx() != c.wantW || dst.Bounds().Dy() != c.wantH {
			t.Errorf("%dx%d became %dx%d, want %dx%d", c.bw, c.bh, dst.Bounds().Dx(), dst.Bounds().Dy(), c.wantW, c.wantH)
		}
	}
}

// DayDir is the path that goes to the report as CopyDir; when it is empty there
// is no report.txt next to the images and the directory on the share is only a
// pile of loose jpegs. It has to be filled as soon as something is really
// written — and stay empty when there was nothing to make, because then the
// report would create an empty day directory.
func TestThumbnailsFillDayDirOnlyWhenSomethingIsThere(t *testing.T) {
	source, target := testSource(t)
	log, _ := captureLog()

	good := putImage(t, source, "2019/IMG_4412.JPG", 200, 150, "jpeg")
	res, err := MakeThumbnails(context.Background(), log, source, target, []DeletedFile{good},
		ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512})
	if err != nil {
		t.Fatalf("making thumbnails failed: %v", err)
	}
	if res.Made != 1 {
		t.Fatalf("made = %d, want 1 (%v)", res.Made, res.Reasons)
	}
	if res.DayDir != target {
		t.Errorf("day directory = %q, want %q; without it no copy of the report goes to the share", res.DayDir, target)
	}

	// Nothing suitable: then DayDir stays empty and the report creates no
	// directory there.
	video := DeletedFile{Raw: "/x/movie.mov", Path: filepath.Join(source.Mountpoint, "movie.mov"), Decodable: true}
	empty, err := MakeThumbnails(context.Background(), log, source, filepath.Join(t.TempDir(), "2026-08-31"),
		[]DeletedFile{video}, ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512})
	if err != nil {
		t.Fatalf("making thumbnails failed: %v", err)
	}
	if empty.DayDir != "" {
		t.Errorf("day directory = %q while nothing was made", empty.DayDir)
	}
}

// The intention and the execution must say the same thing: the payload names
// the count from the plan, the aftercare makes exactly that many.
func TestThumbnailPlanAndExecutionAgree(t *testing.T) {
	source, target := testSource(t)
	log, _ := captureLog()
	var list []DeletedFile
	for i := 0; i < 5; i++ {
		list = append(list, putImage(t, source, filepath.Join("2019", string(rune('a'+i))+".jpg"), 80, 60, "jpeg"))
	}
	list = append(list, DeletedFile{Raw: "/x/movie.mov", Path: filepath.Join(source.Mountpoint, "movie.mov"), Decodable: true})

	opts := ThumbnailOptions{Max: 3, Px: 320, MaxTotalMB: 512}
	plan := PlanThumbnails(target, list, opts)
	if len(plan.Chosen) != 3 || plan.Candidates != 5 {
		t.Fatalf("plan: %d chosen out of %d candidates, want 3 out of 5", len(plan.Chosen), plan.Candidates)
	}
	res, err := MakeThumbnails(context.Background(), log, source, target, list, opts)
	if err != nil {
		t.Fatalf("making thumbnails failed: %v", err)
	}
	if res.Made != len(plan.Chosen) {
		t.Errorf("made = %d, the plan said %d", res.Made, len(plan.Chosen))
	}
}

// When the thumbnail directory is at its software limit nothing is written —
// and then the plan may not promise anything either. The notification would
// otherwise point at a day directory that never comes into being.
func TestThumbnailPlanPromisesNothingAboveTheLimit(t *testing.T) {
	source, _ := testSource(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "filler"), make([]byte, 3<<20), 0o640); err != nil {
		t.Fatalf("filler cannot be written: %v", err)
	}
	target := filepath.Join(root, "2026-08-31")
	photo := putImage(t, source, "2019/IMG_4412.JPG", 80, 60, "jpeg")

	plan := PlanThumbnails(target, []DeletedFile{photo}, ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 2})
	if len(plan.Chosen) != 0 {
		t.Errorf("plan picks %d files while the directory is over the limit", len(plan.Chosen))
	}
	if plan.Message == "" {
		t.Error("there is no message; then it stays silent while nothing is made")
	}

	log, buf := captureLog()
	res, err := MakeThumbnails(context.Background(), log, source, target, []DeletedFile{photo},
		ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Made != 0 || res.Message == "" {
		t.Errorf("made=%d message=%q, want 0 and an explanation", res.Made, res.Message)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the day directory was created anyway (err=%v)", err)
	}
	if !strings.Contains(buf.String(), "too large") {
		t.Errorf("the journal does not say why nothing was made: %s", buf.String())
	}
}

// The notification promised "thumbnails: <dir>" and "thumbnails_planned: 2",
// and if then *every* file drops out, that was the end of it: an empty day
// directory on the share, no side issue, not a word in tomorrow's message.
// While docs/RECOVERY.md says you should wait a moment and they will be there.
func TestNoThumbnailAtAllReportsItselfAndLeavesNoEmptyDir(t *testing.T) {
	source, target := testSource(t)
	log, buf := captureLog()

	// Two files with a valid extension that hold no image: this is the case
	// where the plan promises two and it becomes zero.
	broken := []DeletedFile{
		putFile(t, source, "2019/IMG_4412.JPG", []byte("this is not a jpeg")),
		putFile(t, source, "2019/IMG_4413.JPG", []byte("this is not one either")),
	}
	plan := PlanThumbnails(target, broken, ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512})
	if len(plan.Chosen) != 2 {
		t.Fatalf("the plan chose %d files, want 2 — otherwise this test measures nothing", len(plan.Chosen))
	}

	res, err := MakeThumbnails(context.Background(), log, source, target, broken,
		ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512})
	if err != nil {
		t.Fatalf("making thumbnails returned an error: %v", err)
	}
	if res.Made != 0 {
		t.Fatalf("made = %d, want 0", res.Made)
	}
	if res.Message == "" {
		t.Error("no message was set; then the \"side issues\" automation stays silent about an empty directory")
	}
	if !strings.Contains(res.Message, "not a readable image") {
		t.Errorf("message = %q; the most common reason belongs in it", res.Message)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the empty day directory %s is still there (err=%v); it would sit on the share for sixteen days", target, err)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Error("there is no WARN in the journal about the failed thumbnails")
	}
}

// When there *is* something in the day directory, it stays: deleting is one
// level deep and only of what we recognize ourselves.
func TestEmptyDayDirWithForeignFileStays(t *testing.T) {
	source, target := testSource(t)
	log, _ := captureLog()
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("day directory cannot be created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "note.txt"), []byte("from a human"), 0o640); err != nil {
		t.Fatalf("file cannot be written: %v", err)
	}
	broken := []DeletedFile{putFile(t, source, "2019/IMG_4412.JPG", []byte("not a jpeg"))}
	if _, err := MakeThumbnails(context.Background(), log, source, target, broken,
		ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512}); err != nil {
		t.Fatalf("making thumbnails returned an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "note.txt")); err != nil {
		t.Errorf("the file from a human disappeared: %v", err)
	}
}

// A dry run always writes into the same directory. If the first one succeeds
// (thumbnails plus a copy of the report) and no thumbnail succeeds in the
// second, the report of the previous time stayed behind — os.Remove failed on
// the non-empty directory and that was only visible at DEBUG. In
// <THUMBNAIL_DIR>/dry-run/ there would then be a report without images, without
// any hint that it was old, in exactly the directory somebody is using to check
// whether the scaling works.
func TestSecondDryRunLeavesNoOrphanedReport(t *testing.T) {
	source, _ := testSource(t)
	target := filepath.Join(t.TempDir(), "dry-run")
	log, _ := captureLog()

	// The result of the first dry run: one thumbnail and the copy of the report
	// next to it.
	good := putImage(t, source, "2019/IMG_4411.JPG", 64, 48, "jpeg")
	if _, err := MakeThumbnails(context.Background(), log, source, target, []DeletedFile{good},
		ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512}); err != nil {
		t.Fatalf("first dry run returned an error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "report.txt"), []byte("from the previous dry run\n"), 0o640); err != nil {
		t.Fatalf("copy of the report cannot be written: %v", err)
	}

	// The second dry run: the same directory, but no file is readable.
	broken := []DeletedFile{putFile(t, source, "2019/IMG_4412.JPG", []byte("not a jpeg"))}
	res, err := MakeThumbnails(context.Background(), log, source, target, broken,
		ThumbnailOptions{Max: 24, Px: 320, MaxTotalMB: 512})
	if err != nil {
		t.Fatalf("second dry run returned an error: %v", err)
	}
	if res.Made != 0 {
		t.Fatalf("made = %d, want 0 — otherwise this test measures nothing", res.Made)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		items, _ := os.ReadDir(target)
		names := []string{}
		for _, i := range items {
			names = append(names, i.Name())
		}
		t.Errorf("the dry-run directory is still there with %v; the report of the previous dry run is orphaned", names)
	}
}
