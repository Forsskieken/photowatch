package watch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Time limits per kind of command. `zfs diff` has to walk the whole object list
// of two snapshots and may therefore take much longer than the rest; the other
// commands are metadata work of seconds at most. Without a limit a stuck
// command would leave the run hanging until somebody notices.
const (
	listTimeout     = 60 * time.Second
	diffTimeout     = 10 * time.Minute
	snapshotTimeout = 2 * time.Minute
)

// Lines longer than this should not exist; a path on Linux is at most 4096
// characters, times 4 for the octal escapes of zfs diff, plus some margin.
const maxDiffLine = 64 * 1024

// ErrSnapshotExists means the snapshot name is already there. That is not an
// error: it happens on a second run on the same day, and running again must be
// harmless.
var ErrSnapshotExists = errors.New("snapshot already exists")

// Zfs runs zfs commands. Always with an argument list and an absolute path — no
// shell and no glued-together command string is involved anywhere, because the
// dataset and path names passing through here are input from outside.
type Zfs struct {
	Path string
}

// Snapshot is one line from `zfs list -t snapshot`.
type Snapshot struct {
	Name    string // full: dataset@snapname
	Short   string // only the part after the @
	Created time.Time
}

// DeletedFile is one deleted regular file from `zfs diff`, in both forms. Raw
// is exactly what zfs printed (with the octal escapes); Path is the decoded
// form that passed the round-trip check.
//
// When Decodable is false, Path holds the same as Raw. That is deliberate and
// not sloppy: sorting, filtering and counting then work for *all* files, while
// the flag decides what may further happen with them. A path that is not
// decodable does not go into the restore script, gets no thumbnail, and appears
// in the report with a ! in front of it in its raw form.
type DeletedFile struct {
	Raw       string
	Path      string
	Decodable bool
}

// DiffResult is the count of one `zfs diff`, already filtered on the path
// prefix.
type DiffResult struct {
	Deleted      []DeletedFile // deleted regular files, sorted by path
	DeletedDirs  int
	DeletedOther int // symlinks, sockets, devices: counted, no alert
	Renamed      int
	Added        int
	Modified     int
	Skipped      int // lines that fell outside the path prefix
	Unparsed     int // lines that did not match the expected shape
	NotDecodable int // deleted files whose path could not be converted
}

// MediaFiles and SidecarFiles split the deleted files into "real photos" and
// "bookkeeping of another program". Both count towards the threshold — nothing
// disappeared that does not matter — but in the notification it makes quite a
// difference whether 43 photos are gone or 22 photos with their 21 XMP
// sidecars.
func (d *DiffResult) MediaFiles() int {
	return len(d.Deleted) - d.SidecarFiles()
}

func (d *DiffResult) SidecarFiles() int {
	n := 0
	for _, v := range d.Deleted {
		if IsSidecar(v.Path) {
			n++
		}
	}
	return n
}

// IsSidecar recognizes files that belong to a photo but are not a photo
// themselves: the XMP sidecars Immich puts next to every image, the .nfo files
// of older media managers, and Windows' Thumbs.db. Deliberately a short,
// explicit list: anything we do not know counts as a media file, because
// wrongly calling an unknown extension "bookkeeping" is the mistake you do not
// want to make.
func IsSidecar(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if name == "thumbs.db" {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".xmp", ".nfo":
		return true
	}
	return false
}

func (z Zfs) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, z.Path, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), zfsError(args, err, errBuf.String(), ctx.Err())
	}
	return out.Bytes(), nil
}

// zfsError builds one error message that shows *which* command failed, with
// which arguments and with which stderr. Without those three you are guessing
// in journalctl.
func zfsError(args []string, err error, stderr string, ctxErr error) error {
	stderr = shortText(strings.TrimSpace(strings.ReplaceAll(stderr, "\n", " | ")), 500)
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("zfs %s took too long and was aborted (stderr: %s)", strings.Join(args, " "), stderr)
	}
	return fmt.Errorf("zfs %s failed: %w (stderr: %s)", strings.Join(args, " "), err, stderr)
}

func shortText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// DatasetExists checks before anything else whether the configured dataset is
// there. A typo in DATASET would otherwise only show up as a cryptic diff
// error.
func (z Zfs) DatasetExists(ctx context.Context, dataset string) error {
	_, err := z.run(ctx, listTimeout, "list", "-H", "-o", "name", dataset)
	return err
}

// Mountpoint is needed for the rsync example in the report: yesterday's
// snapshot lives under <mountpoint>/.zfs/snapshot/<name>/.
func (z Zfs) Mountpoint(ctx context.Context, dataset string) (string, error) {
	out, err := z.run(ctx, listTimeout, "get", "-H", "-o", "value", "mountpoint", dataset)
	if err != nil {
		return "", err
	}
	mp := strings.TrimSpace(string(out))
	// "legacy" or "none" means the dataset is not mounted through ZFS; we then
	// cannot compute the restore path, and the report says so itself.
	if mp == "" || mp == "-" || mp == "legacy" || mp == "none" {
		return "", fmt.Errorf("dataset %s has mountpoint %q; the restore path cannot be derived", dataset, mp)
	}
	return mp, nil
}

// SnapshotList returns all snapshots of the dataset itself. `-r` also yields
// those of child datasets; we filter those out, because a snapshot of
// tank/photos/test does not belong in our count and certainly not in the
// pruning.
//
// The second number is the count of lines that did concern this dataset but
// could not be parsed. That may not disappear silently: an unreadable line
// means we are missing a snapshot, and a missed snapshot can be both the
// reference for the diff and a candidate for pruning. The caller logs it as a
// WARN.
func (z Zfs) SnapshotList(ctx context.Context, dataset string) ([]Snapshot, int, error) {
	// -p gives creation as a unix time instead of localized text; that is the
	// only format that does not depend on the locale.
	out, err := z.run(ctx, listTimeout, "list", "-H", "-p", "-o", "name,creation", "-t", "snapshot", "-r", dataset)
	if err != nil {
		return nil, 0, err
	}
	return parseSnapshotList(bytes.NewReader(out), dataset)
}

func parseSnapshotList(r io.Reader, dataset string) ([]Snapshot, int, error) {
	var snaps []Snapshot
	unparsed := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxDiffLine)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			unparsed++
			continue
		}
		name := fields[0]
		ds, short, ok := strings.Cut(name, "@")
		if !ok {
			unparsed++
			continue
		}
		if ds != dataset {
			// A snapshot of a child dataset: does not belong here and is not an
			// error. -r yields those too, we only count our own.
			continue
		}
		sec, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil {
			// Without a reliable creation time we cannot decide on retention;
			// this snapshot is left out of consideration, but it does count as
			// unparsed so that it stands out.
			unparsed++
			continue
		}
		snaps = append(snaps, Snapshot{Name: name, Short: short, Created: time.Unix(sec, 0)})
	}
	if err := sc.Err(); err != nil {
		return nil, unparsed, fmt.Errorf("output of zfs list not readable: %w", err)
	}
	return snaps, unparsed, nil
}

// LatestWithPrefix finds the newest snapshot whose short name starts with the
// prefix. Deliberately looser than the rule we delete by: diffing against the
// snapshot of another snapshotter is allowed, throwing it away is not.
func LatestWithPrefix(snaps []Snapshot, prefix string) (Snapshot, bool) {
	var best Snapshot
	found := false
	for _, s := range snaps {
		if !strings.HasPrefix(s.Short, prefix) {
			continue
		}
		if !found || s.Created.After(best.Created) ||
			(s.Created.Equal(best.Created) && s.Name > best.Name) {
			best, found = s, true
		}
	}
	return best, found
}

// Diff compares the snapshot with the current state of the dataset. The output
// is read line by line: after a big cleanup that is hundreds of thousands of
// lines, and we do not want those in memory in one go.
func (z Zfs) Diff(ctx context.Context, previousSnapshot, dataset, pathPrefix string) (*DiffResult, error) {
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	args := []string{"diff", "-H", "-F", previousSnapshot, dataset}
	cmd := exec.CommandContext(ctx, z.Path, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("could not attach the output of zfs diff: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, zfsError(args, err, errBuf.String(), ctx.Err())
	}

	res, readErr := parseDiff(pipe, pathPrefix)
	if readErr != nil {
		// Do not simply call Wait() here: zfs is still writing to a pipe nobody
		// drains, and Wait would block until the ten-minute timeout. Closing the
		// read end gives zfs an EPIPE and cancelling the context ends it anyway;
		// only then Wait, purely to reap the process. The error from Wait is a
		// consequence of our own intervention and says nothing — readErr is the
		// real reason and that is what we pass on.
		_ = pipe.Close()
		cancel()
		_ = cmd.Wait()
		return nil, readErr
	}
	// Always call Wait: otherwise the process stays behind as a zombie and we
	// leak the pipe.
	if err := cmd.Wait(); err != nil {
		return nil, zfsError(args, err, errBuf.String(), ctx.Err())
	}
	return res, nil
}

// parseDiff reads the output of `zfs diff -H -F`. Lines are tab separated:
// character, type, path, and for a rename a fourth field with the new path.
// Splitting on tab and not on whitespace is essential: a file name may contain
// spaces.
//
// The octal escapes zfs puts in paths (\0040 for a space) are translated back
// here, with the round-trip check from zfspath.go as the safeguard: decode,
// re-encode and compare byte for byte. If that fails, the path stays in its raw
// form and does not travel to the restore script.
//
// Filtering on the path prefix happens on the decoded path. That removes a
// latent bug straight away: on the raw form a PATH_PREFIX with a space in it
// would never have matched, because there the space is \0040.
func parseDiff(r io.Reader, pathPrefix string) (*DiffResult, error) {
	res := &DiffResult{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxDiffLine)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || len(fields[0]) != 1 || len(fields[1]) != 1 {
			res.Unparsed++
			continue
		}
		mark, kind, raw := fields[0], fields[1], fields[2]

		path, decodable := RoundTripZfsPath(raw)
		if !decodable {
			// Without a reliable translation the raw form is all we have; we use
			// that to filter and sort as well.
			path = raw
		}

		if !withinPathPrefix(pathPrefix, path) {
			res.Skipped++
			continue
		}

		switch mark {
		case "-":
			switch kind {
			case "F":
				res.Deleted = append(res.Deleted, DeletedFile{Raw: raw, Path: path, Decodable: decodable})
				if !decodable {
					res.NotDecodable++
				}
			case "/":
				res.DeletedDirs++
			default:
				// Symlinks, sockets, fifos, devices. They count in the report but
				// cause no alert: a deleted directory with 300 photos in it also
				// yields 300 separate F lines, and *that* is what we want to
				// count.
				res.DeletedOther++
			}
		case "+":
			res.Added++
		case "M":
			res.Modified++
		case "R":
			// A rename is not a deletion: the file is still there.
			res.Renamed++
		default:
			res.Unparsed++
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("output of zfs diff not readable: %w", err)
	}
	// `zfs diff` gives no guaranteed order. Sorting groups the files per
	// directory, which makes the report readable, the examples in the
	// notification meaningful, and the even sample for the thumbnails a real
	// cross-section instead of a grab from one directory.
	sort.SliceStable(res.Deleted, func(i, j int) bool {
		if res.Deleted[i].Path == res.Deleted[j].Path {
			return res.Deleted[i].Raw < res.Deleted[j].Raw
		}
		return res.Deleted[i].Path < res.Deleted[j].Path
	})
	return res, nil
}

// Snapshot creates the snapshot. If the name already exists, ErrSnapshotExists
// comes back so that the caller can decide about it.
func (z Zfs) Snapshot(ctx context.Context, fullName string) error {
	if err := checkSnapshotName(fullName); err != nil {
		return err
	}
	_, err := z.run(ctx, snapshotTimeout, "snapshot", fullName)
	if err != nil {
		// zfs has no exit code of its own for "already exists", only this text
		// on stderr. Hence the string comparison; it lives in this one place.
		if strings.Contains(err.Error(), "dataset already exists") {
			return fmt.Errorf("%s: %w", fullName, ErrSnapshotExists)
		}
		return err
	}
	return nil
}

// Destroy throws away one snapshot. This is the only irreversible thing this
// program does; the actual choice of *which* name arrives here lives in
// retention.go. The check below is a second lock on the same door: without an @
// in the name, `zfs destroy` would wipe the whole dataset.
func (z Zfs) Destroy(ctx context.Context, fullName string) error {
	if err := checkSnapshotName(fullName); err != nil {
		return err
	}
	_, err := z.run(ctx, snapshotTimeout, "destroy", fullName)
	return err
}

func checkSnapshotName(name string) error {
	ds, short, ok := strings.Cut(name, "@")
	if !ok || ds == "" || short == "" {
		return fmt.Errorf("refused: %q is not a snapshot name of the form dataset@name", name)
	}
	if strings.ContainsAny(name, "%, \t\n") {
		// % and , are the range separators of `zfs destroy`; with those, one
		// name could wipe several snapshots.
		return fmt.Errorf("refused: snapshot name %q contains a character that could denote a range", name)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("refused: snapshot name %q starts with - and would be read as a flag", name)
	}
	if strings.Count(name, "@") != 1 {
		return fmt.Errorf("refused: snapshot name %q contains more than one @", name)
	}
	return nil
}

// SortNewestFirst is a helper for the retention and the log lines.
func SortNewestFirst(snaps []Snapshot) {
	sort.SliceStable(snaps, func(i, j int) bool {
		if snaps[i].Created.Equal(snaps[j].Created) {
			return snaps[i].Name > snaps[j].Name
		}
		return snaps[i].Created.After(snaps[j].Created)
	})
}
