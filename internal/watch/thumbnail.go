package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	// Only for the decoders. JPEG is also used directly (for writing), PNG and
	// GIF only so that they can be read.
	_ "image/gif"
	_ "image/png"
)

// Limits on what we open. Go's image libraries are memory safe, but a broken or
// malicious JPEG can still provoke an enormous allocation: the header says
// "40000 by 40000 pixels" and then 6.4 GB sits in memory before a single pixel
// has been read. Hence DecodeConfig first (which only reads the header) and
// only then Decode.
const (
	maxSourceBytes = 200 << 20 // 200 MB; a photo larger than that is not a photo

	// The limit that really matters, and why it is in bytes and not in pixels.
	// This used to say "at most 80 megapixels", but the number of bytes per
	// pixel ranges from 1 (a paletted GIF) to 8 (a PNG with 16 bits per channel,
	// which image/png returns as NRGBA64). 80 megapixels is then 80 MB or 640
	// MB, and the latter hits the MemoryMax=1G of the systemd unit — after which
	// the process is killed in the middle of a run. Such a file need not be
	// malicious: a scan or an export produces 16-bit PNGs.
	//
	// 256 MB is well below MemoryMax, even with the decoder's work buffers and
	// the slack the GC takes, and it lets every real photo through: a JPEG of
	// 100 megapixels comes to at most 3 bytes per pixel as YCbCr (300 MB — that
	// one is rejected, and rightly so), one of 80 megapixels to 240 MB. At 8
	// bytes per pixel, 32 megapixels remain.
	maxSourceDecodeBytes = 256 << 20

	maxThumbnailDuration = 2 * time.Minute
	// Quality 70 is more than enough for an image of 320 pixels and keeps a
	// thumbnail under 30 kB.
	thumbnailQuality = 70
)

// Extensions we can make a thumbnail of. This is an allowlist: everything not
// in it is skipped with a reason in the report. HEIC, raw and video are absent
// on purpose — there is no decoder for those in the standard library, and
// ffmpeg does not go on the hypervisor.
var thumbnailExtensions = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
}

// thumbnailName matches the names this file makes itself: NNN- followed by a
// cleaned base name and .jpg. The cleanup uses the same pattern; if there is
// anything in the day directory that does not match it, that directory stays.
var thumbnailName = regexp.MustCompile(`^\d{3}-[A-Za-z0-9._-]{1,60}\.jpg$`)

// maxBaseName is the length the cleaned base name is cut off at. It also
// appears in thumbnailName above; change them together.
const maxBaseName = 60

// SnapshotSource says where yesterday's files still are.
type SnapshotSource struct {
	Mountpoint string // mountpoint of the watched dataset
	Dir        string // <mountpoint>/.zfs/snapshot/<short snapshot name>
}

// ThumbnailOptions are the configurable limits; they come from the config.
type ThumbnailOptions struct {
	Max        int // at most this many thumbnails per run
	Px         int // long side in pixels
	MaxTotalMB int // above this size of the thumbnail directory we make no more
}

// ThumbnailResult is what the report and the payload get to see of it.
type ThumbnailResult struct {
	DayDir     string
	Made       int
	Candidates int            // files a thumbnail *could* be made of
	Numbers    map[string]int // raw path -> sequence number, for the [001] marker in the report
	Reasons    map[string]int // reason -> number of skipped candidates
	Message    string         // short text for the payload when something went wrong
}

// ThumbnailPlan is what can be made from a list of deleted files, without a
// single file being opened for it: which ones are up, why the rest drops out,
// and whether anything may be written at all.
//
// It exists separately from MakeThumbnails because the notification goes out
// before the scaling (see runAftercare in run.go). The payload must therefore
// already be able to say where the images will be, how many there will be and
// whether anything is in the way. Both callers use this one function, so that
// the intention and the execution cannot drift apart; it only reads and can
// therefore be run twice per run without harm.
type ThumbnailPlan struct {
	Chosen     []DeletedFile  // a thumbnail is attempted for these
	Candidates int            // files a thumbnail *could* be made of
	Reasons    map[string]int // reason -> number of rejected candidates
	Message    string         // not empty = nothing is written, and this is why
}

// PlanThumbnails picks the even sample and does the two checks that open
// nothing: is there anything suitable, and is the thumbnail directory already
// at its software limit.
func PlanThumbnails(target string, candidates []DeletedFile, opts ThumbnailOptions) ThumbnailPlan {
	plan := ThumbnailPlan{Reasons: map[string]int{}}

	suitable := make([]DeletedFile, 0, len(candidates))
	for _, v := range candidates {
		switch {
		case !v.Decodable:
			plan.Reasons["path not decodable"]++
		case IsSidecar(v.Path):
			plan.Reasons["sidecar file, not a photo"]++
		case !thumbnailExtensions[strings.ToLower(filepath.Ext(v.Path))]:
			// Videos and HEIC end up here. They *are* in the report, with this
			// reason next to them, so that nobody thinks they were forgotten.
			plan.Reasons["not jpg/png/gif"]++
		default:
			suitable = append(suitable, v)
		}
	}
	plan.Candidates = len(suitable)
	if len(suitable) == 0 {
		return plan
	}

	// The software limit. It always hits before the ZFS quota, so that the
	// ordinary path is a tidy WARN and the quota stays the emergency brake for
	// the case where this code is wrong. The size of the whole thumbnail
	// directory counts, not only today's: what matters is what sits on the
	// share.
	//
	// A directory that cannot be measured (it does not exist yet, or it is not
	// readable) is not a reason to stop here: writing will then fail by itself,
	// *with* a reason per file.
	root := filepath.Dir(target)
	if mb, err := DirSizeMB(root); err == nil && opts.MaxTotalMB > 0 && mb >= opts.MaxTotalMB {
		// Chosen stays empty: nothing is written, and then the message may not
		// point at a day directory that will not come into being.
		plan.Message = fmt.Sprintf("thumbnail directory is %d MB (limit %d); no new thumbnails made", mb, opts.MaxTotalMB)
		return plan
	}
	plan.Chosen = evenSample(suitable, opts.Max)
	return plan
}

// MakeThumbnails scales an even sample of the vanished photos from the snapshot
// into the day directory on the share.
//
// Ground rule of this whole part: none of it may hold up the notification. The
// alert is the product, a thumbnail is comfort. An error on one file is
// therefore not a failure but a line in the report; only an error that hits
// *everything* (the day directory cannot be created) comes back as an error,
// and the caller handles that as a WARN too.
//
// It only runs after the notification: decoding arbitrary image files as root
// is the riskiest thing this program does, and that must never sit between the
// irreversible snapshot and sending the alert.
func MakeThumbnails(ctx context.Context, log *slog.Logger, source SnapshotSource, target string, candidates []DeletedFile, opts ThumbnailOptions) (ThumbnailResult, error) {
	start := time.Now()
	plan := PlanThumbnails(target, candidates, opts)
	// DayDir is deliberately filled only at the bottom, and only when there
	// really is a thumbnail in that directory. It used to be set here, and then
	// it stayed set on the paths that return early below (no suitable
	// candidates, or the directory is over THUMBNAIL_MAX_MB). The caller passes
	// DayDir on as CopyDir to the report, and that would then create an empty
	// day directory with only a report.txt in it — in the second case even in a
	// directory we had just decided was too large.
	res := ThumbnailResult{
		Numbers:    map[string]int{},
		Reasons:    plan.Reasons,
		Candidates: plan.Candidates,
		Message:    plan.Message,
	}
	if plan.Candidates == 0 {
		return res, nil
	}
	if plan.Message != "" {
		log.Warn("thumbnail directory is too large; no new thumbnails are made",
			"dir", filepath.Dir(target), "message", plan.Message, "limit_mb", opts.MaxTotalMB,
			"consequence", "the notification and the report arrive as usual, only without images",
			"check", "whether the cleanup still runs (artifacts_mb in the notification) and otherwise raise THUMBNAIL_MAX_MB")
		return res, nil
	}

	if err := makeDirWithGroup(log, target); err != nil {
		return res, err
	}
	// A second run on the same day may not leave a number 003 behind that points
	// at a different photo than the report says. So first remove our own result
	// from an earlier round — the thumbnails and the copy of the report —
	// through the same guarded delete function as the rest of the program.
	if n, err := emptyDayDir(log, target); err != nil {
		log.Warn("the result of an earlier round in the day directory cannot be cleaned up; the numbers may get mixed up",
			"dir", target, "error", err)
	} else if n > 0 {
		log.Debug("result of an earlier round removed from the day directory", "dir", target, "files", n)
	}

	// A time limit of its own on top of the run's: the scaling is the only part
	// that grows with the size of the files, and it may never hold up the
	// notification.
	ctx, cancel := context.WithTimeout(ctx, maxThumbnailDuration)
	defer cancel()

	number := 0
	for _, v := range plan.Chosen {
		if ctx.Err() != nil {
			res.Reasons["out of time"]++
			continue
		}
		number++
		name := fmt.Sprintf("%03d-%s.jpg", number, cleanBaseName(v.Path))
		path, err := safeFilePath(target, name)
		if err != nil {
			// Cannot happen as long as cleanBaseName uses an allowlist, but it is
			// the cheapest check in the whole file and the one place where a file
			// name arises from a path from outside.
			number--
			res.Reasons["file name not safe"]++
			log.Warn("thumbnail name would end up outside the day directory; skipped", "name", name, "error", err)
			continue
		}
		reason, err := oneThumbnail(source, v, path, opts.Px, log)
		if err != nil {
			number--
			res.Reasons[reason]++
			log.Debug("no thumbnail made", "reason", reason, "path", cleanText(v.Path, maxExampleLength), "error", err)
			if isDiskFull(err) {
				res.Message = "thumbnail directory is at its quota; clean up or raise `zfs set quota`"
				log.Warn("the thumbnail directory is at its quota", "dir", target, "error", err,
					"check", "zfs get quota,used on the dataset of the thumbnail directory")
				break
			}
			continue
		}
		res.Numbers[v.Raw] = number
		res.Made++
	}

	// Only here, and only when there really is an image in that directory. The
	// caller passes DayDir on as CopyDir to the report; if it were already
	// filled above, a run without suitable candidates or a run that stranded on
	// the size limit would still create a day directory with only a report.txt
	// in it.
	if res.Made > 0 {
		res.DayDir = target
	} else {
		// Not one of the chosen files became an image. The notification did
		// promise a path and a count, so this may not stay silent: without this
		// line the reader opens a directory that is not there (or that is empty),
		// while docs/RECOVERY.md tells them to wait a moment.
		reason, count := mostCommonReason(res.Reasons)
		if res.Message == "" {
			// Only when there is no more specific message yet: the quota one says
			// more than "could not be written (24x)".
			res.Message = fmt.Sprintf("not a single thumbnail succeeded out of the %d chosen photos; mostly: %s (%dx)",
				len(plan.Chosen), reason, count)
		}
		log.Warn("not a single thumbnail was made while the notification pointed at them",
			"chosen", len(plan.Chosen), "dir", target, "most_common_reason", reason, "skipped", res.Reasons,
			"consequence", "the day directory is removed again and the reason travels in the next run's notification",
			"check", "whether the snapshot directory is readable (ls on the directory above) and whether this kind of file can be decoded")
		// The directory we created ourselves a few lines above and that stayed
		// empty. If you leave it, it sits on the share for sixteen days and the
		// notification points at an empty directory.
		removeEmptyDayDir(log, target)
	}

	log.Info("thumbnails made",
		"count", res.Made, "candidates", res.Candidates, "dir", target,
		"skipped", res.Reasons, "duration_s", rounded(time.Since(start).Seconds()))
	return res, nil
}

// mostCommonReason returns the reason that occurred most often, with its count.
// On a tie the alphabetically first one, so that the same outcome is not
// reported two different ways — a map has no order.
func mostCommonReason(reasons map[string]int) (string, int) {
	best, count := "", 0
	for reason, n := range reasons {
		if n > count || (n == count && reason < best) {
			best, count = reason, n
		}
	}
	if best == "" {
		return "unknown", 0
	}
	return best, count
}

// evenSample picks at most max files, spread evenly over the (sorted) list. For
// 400 deleted files you do not want 400 images but an impression of *what* is
// gone — and because the list is sorted by path, spread evenly also means:
// spread over the directories.
func evenSample(list []DeletedFile, max int) []DeletedFile {
	if max <= 0 {
		return nil
	}
	if len(list) <= max {
		return list
	}
	out := make([]DeletedFile, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, list[i*len(list)/max])
	}
	return out
}

// cleanBaseName turns a path from outside into a file name we could have chosen
// ourselves: only [A-Za-z0-9._-], everything else becomes _. An allowlist and
// not a denylist, because the original path is used here to build a file path
// and that is exactly the spot where a denylist eventually forgets something.
func cleanBaseName(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	for _, c := range []byte(base) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxBaseName {
			break
		}
	}
	name := b.String()
	// A name starting with a dot would give a hidden file and does not match
	// thumbnailName; "." and ".." are no name at all.
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "without-name"
	}
	return name
}

// oneThumbnail makes one image. The returned text is the reason for the report;
// it is deliberately short and without a path, because it is counted.
//
// The recover below shields the decoding of input from outside. The decoders in
// the standard library are memory safe but not panic free — panics in image/gif
// and image/png have been fixed over the years — and this program runs as root
// over arbitrary files from the archive. An error on one file is not a failure
// but a line in the report; without this recover that held for an error and not
// for a panic, and then one file takes down the whole run.
//
// What a recover does not catch and therefore does not promise: an OOM kill by
// the cgroup and a stack overflow. The byte limit above is the answer to those.
func oneThumbnail(source SnapshotSource, v DeletedFile, targetPath string, px int, log *slog.Logger) (reason string, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			reason = "decoder crashed (panic)"
			retErr = fmt.Errorf("panic while processing %s: %v", targetPath, r)
			log.Warn("an image file from the snapshot took down a decoder; only this file is skipped",
				"path", cleanText(v.Path, maxExampleLength), "panic", fmt.Sprint(r),
				"consequence", "the run continues; the file is still in the report")
		}
	}()

	sourcePath, err := pathInSnapshot(source, v.Path)
	if err != nil {
		return "outside the snapshot directory", err
	}
	// Lstat and not Stat: we do not follow a symlink in the snapshot. If it
	// points outside the dataset, we would read a file we did not mean.
	fi, err := os.Lstat(sourcePath)
	if err != nil {
		return "not (or no longer) in the snapshot", err
	}
	if !fi.Mode().IsRegular() {
		return "not a regular file in the snapshot", fmt.Errorf("%s is %s", sourcePath, fi.Mode().Type())
	}
	if fi.Size() > maxSourceBytes {
		return "file too large", fmt.Errorf("%d bytes, limit %d", fi.Size(), maxSourceBytes)
	}

	f, err := os.Open(sourcePath)
	if err != nil {
		return "cannot be opened", err
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "not a readable image", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return "not a readable image", fmt.Errorf("dimensions %dx%d", cfg.Width, cfg.Height)
	}
	// int64 and not int: the width and the height come from the header of a file
	// from outside, and on a 32-bit host the product could otherwise wrap into a
	// small positive number and dodge the limit.
	bpp := bytesPerPixel(cfg.ColorModel)
	needed := int64(cfg.Width) * int64(cfg.Height) * bpp
	if needed > maxSourceDecodeBytes {
		return "needs too much memory", fmt.Errorf("%dx%d pixels at %d bytes = %d bytes, limit %d",
			cfg.Width, cfg.Height, bpp, needed, int64(maxSourceDecodeBytes))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "cannot be read", err
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return "image cannot be decoded", err
	}

	small := boxScale(img, px)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: thumbnailQuality}); err != nil {
		return "cannot be encoded as JPEG", err
	}
	if err := writeAtomic(log, targetPath, modeReport, buf.Bytes()); err != nil {
		return "cannot be written", err
	}
	return "", nil
}

// bytesPerPixel estimates how much memory one pixel of this image costs once
// Decode unpacks it. The limit has to be in bytes and not in pixels, because
// the same number of pixels costs between 1 and 8 times as much memory,
// depending on the color model the decoder returns.
//
// It is an upper bound, not an exact measurement. Two examples why: a YCbCr
// JPEG with 4:2:0 subsampling actually uses one and a half bytes per pixel and
// three here, and a paletted GIF keeps a palette of at most 1 kB next to its
// bytes per pixel. Estimating too high is the safe side; too low would make the
// limit worthless.
//
// An unknown color model gets the most expensive value. That can only happen if
// a decoder is ever added, and then being careful is the right answer.
func bytesPerPixel(cm color.Model) int64 {
	// A paletted image (GIF, and PNG with a palette) returns none of the fixed
	// models but the color.Palette itself; that is by definition at most 256
	// colors and therefore costs one byte per pixel.
	if _, paletted := cm.(color.Palette); paletted {
		return 1
	}
	switch cm {
	case color.GrayModel, color.AlphaModel:
		return 1
	case color.Gray16Model, color.Alpha16Model:
		return 2
	case color.YCbCrModel:
		// image.YCbCr keeps three planes; at 4:4:4 that is three bytes per pixel,
		// at 4:2:2 or 4:2:0 less. Three is the upper bound.
		return 3
	case color.RGBAModel, color.NRGBAModel, color.CMYKModel, color.NYCbCrAModel:
		return 4
	case color.RGBA64Model, color.NRGBA64Model:
		// This is the case this function exists for: a PNG with 16 bits per
		// channel comes back as (N)RGBA64 and costs eight times what the pixel
		// count suggests.
		return 8
	default:
		return 8
	}
}

// pathInSnapshot builds the path to the file as it was yesterday, and checks
// that the result stays inside the snapshot directory. The same check
// relativeWithin does on the write side, extended to the read side: a path with
// .. in it or a path outside the mountpoint is refused instead of silently
// reading elsewhere.
func pathInSnapshot(source SnapshotSource, path string) (string, error) {
	rel, ok := relativeWithin(source.Mountpoint, path)
	if !ok {
		return "", fmt.Errorf("path does not fall under the mountpoint %s", source.Mountpoint)
	}
	if rel == "" {
		return "", errors.New("empty path inside the dataset")
	}
	snapDir := filepath.Clean(source.Dir)
	full := filepath.Join(snapDir, rel)
	if full != snapDir && !strings.HasPrefix(full, snapDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%s falls outside the snapshot directory %s", full, snapDir)
	}
	return full, nil
}

// boxScale shrinks with a box filter: every target pixel is the average of the
// block of source pixels that falls on it. That is simpler than Lanczos or even
// bilinear, and that is the point here: the alternative was
// golang.org/x/image/draw, and that would be the first go.sum of this project.
// For an image of 320 pixels on which you have to be able to see "is this a
// christmas tree or a wedding photo", the difference is not worth the
// dependency.
//
// It never enlarges: if the source is already smaller than the requested long
// side, it stays at full size.
func boxScale(src image.Image, longSide int) *image.RGBA {
	b := src.Bounds()
	bw, bh := b.Dx(), b.Dy()
	dw, dh := bw, bh
	if bw >= bh && bw > longSide {
		dw = longSide
		dh = bh * longSide / bw
	} else if bh > bw && bh > longSide {
		dh = longSide
		dw = bw * longSide / bh
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0 := b.Min.Y + y*bh/dh
		y1 := b.Min.Y + (y+1)*bh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := b.Min.X + x*bw/dw
			x1 := b.Min.X + (x+1)*bw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, bl, a := src.At(sx, sy).RGBA()
					sr += uint64(r)
					sg += uint64(g)
					sb += uint64(bl)
					sa += uint64(a)
					n++
				}
			}
			r, g, bl, a := sr/n, sg/n, sb/n, sa/n
			// RGBA() returns premultiplied values (r <= a). JPEG has no
			// transparency, so we composite the average onto white: without this
			// step a PNG with a transparent background turns black.
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8((r + (0xffff - a)) >> 8)
			dst.Pix[i+1] = uint8((g + (0xffff - a)) >> 8)
			dst.Pix[i+2] = uint8((bl + (0xffff - a)) >> 8)
			dst.Pix[i+3] = 0xff
		}
	}
	return dst
}
