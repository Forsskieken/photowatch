package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Above this number of lines we truncate the list. A report of a million lines
// helps nobody and fills /var/log; whoever lost that many files first wants to
// know *that* it happened. The full list can always be regenerated with `zfs
// diff` as long as the snapshot is there, and the report says so.
const maxReportLines = 50000

// Report and state files may be read by the group of their directory (640) so
// that an admin can look at them without sudo. *Which* group that is is not in
// this code: every file and directory inherits the group of the directory above
// it, and that is set during installation.
const (
	modeReport = 0o640
	modeDir    = 0o750
)

// maxPathText is the length a path in the report is truncated at. Well above
// PATH_MAX on Linux (4096), so that in practice nothing is ever cut off.
const maxPathText = 4096

// ReportData is everything that goes into the list file.
type ReportData struct {
	// Name is the file name of this report, chosen by ChooseArtifactSlot. It is
	// here and not derived from Now, so that the path in the notification and
	// the path the report ends up at are guaranteed to be the same — also on a
	// second run on the same day and on a dry run.
	Name             string
	Dataset          string
	PathPrefix       string
	PreviousSnapshot string // full name, empty on the first run
	PreviousShort    string
	PreviousTime     time.Time
	NewSnapshot      string
	Now              time.Time
	Mountpoint       string // empty when it could not be determined
	Threshold        int
	Alert            bool
	DryRun           bool
	Diff             *DiffResult

	// Until when the files can be recovered: the creation date of the previous
	// snapshot plus KEEP_DAYS. This is the most important line of the report —
	// without that date nobody knows there is any hurry.
	RecoverUntil time.Time

	Restore   RestoreResult
	Thumbnail ThumbnailResult

	// CopyDir is the day directory on the share a copy of this report goes to,
	// so that it sits next to the thumbnails. Empty = no copy.
	CopyDir string
}

// The name of the directory where a dry run can put its artifacts, under
// REPORT_DIR and under THUMBNAIL_DIR. See ChooseArtifactSlot.
const dryRunDir = "dry-run"

// How many runs on one day can each get their own set of artifacts. More than
// this is not a second run but a loop; then writing no artifact at all is
// better than putting down hundreds.
const maxRunsPerDay = 99

// ArtifactSlot says where the four artifacts of this run go and what they are
// called: the report, the restore script, the list and the day directory with
// thumbnails. It is chosen once per run (ChooseArtifactSlot) and used
// everywhere after that, instead of being derived from the date four separate
// times.
//
// Why that is needed. The artifacts were named after the *date*, but they
// belong to a reference snapshot. If the watch ran at 08:15 and 43 files
// disappeared compared to yesterday's snapshot, that is in restore-<today>.sh
// and in the day directory <today> — and that path is in the notification on
// the phone. If another run happens at 10:30, it compares against this
// morning's snapshot, finds two entirely different files, and overwrote that
// same script and the same day directory. Whoever then follows the 08:15
// message restores 2 of the 43 files and sees nothing of it.
//
// The solution is the name: the second run of a day gets <date>-2, the third
// <date>-3. Nothing is ever overwritten, and both runs keep a consistent set.
// The cleanup recognizes that shape (see cleanup.go); the date for the
// retention stays the first part of the name.
//
// A dry run gets no stamp but a directory of its own, and that is the second
// reason this type exists: a dry run promises to create, change or delete
// nothing. It therefore writes into <REPORT_DIR>/dry-run and
// <THUMBNAIL_DIR>/dry-run, with fixed names, so that it only replaces its own
// previous result and never touches a real artifact.
type ArtifactSlot struct {
	Dir    string // where the report, restore script and list go
	DayDir string // directory for the thumbnails; empty = no thumbnails this run
	Stamp  string // "2026-09-01", "2026-09-01-2", …; empty on a dry run
	DryRun bool
}

// suffix is what comes after "deleted" and "restore". On a dry run that is
// nothing: that directory holds only one set and the names there must
// specifically *not* match the cleanup pattern.
func (a ArtifactSlot) suffix() string {
	if a.Stamp == "" {
		return ""
	}
	return "-" + a.Stamp
}

func (a ArtifactSlot) ReportName() string { return "deleted" + a.suffix() + ".txt" }
func (a ArtifactSlot) ScriptName() string { return "restore" + a.suffix() + ".sh" }
func (a ArtifactSlot) ListName() string   { return "restore" + a.suffix() + ".list" }

// ReportPath is the full path of the report. It is separate because the payload
// names the path while the file is only written after the notification (see
// runAftercare in main.go): that way the notification can never hold a
// different path than where the report ends up.
func (a ArtifactSlot) ReportPath() (string, error) {
	return safeFilePath(a.Dir, a.ReportName())
}

// First reports whether this is the first set of artifacts of today. If not,
// that belongs in the message: the notification then points at names with a -2
// behind them and nobody should have to wonder why.
func (a ArtifactSlot) First(now time.Time) bool {
	return a.DryRun || a.Stamp == now.Format("2006-01-02")
}

// ChooseArtifactSlot finds the slot this run can put its artifacts in.
//
// For a real run that is the first stamp of today for which none of the four
// names already exists. That is deliberately an existence check and not a
// comparison with the reference snapshot: the artifacts are only written after
// today's snapshot has been made, so if a set of today is already there, it has
// by definition a different reference than this run.
func ChooseArtifactSlot(reportDir, thumbnailDir string, now time.Time, dryRun bool) (ArtifactSlot, error) {
	if dryRun {
		dir, err := safeFilePath(reportDir, dryRunDir)
		if err != nil {
			return ArtifactSlot{}, err
		}
		slot := ArtifactSlot{Dir: dir, DryRun: true}
		if thumbnailDir != "" {
			dayDir, err := safeFilePath(thumbnailDir, dryRunDir)
			if err != nil {
				return ArtifactSlot{}, err
			}
			slot.DayDir = dayDir
		}
		return slot, nil
	}

	base := now.Format("2006-01-02")
	for n := 1; n <= maxRunsPerDay; n++ {
		stamp := base
		if n > 1 {
			stamp = fmt.Sprintf("%s-%d", base, n)
		}
		slot := ArtifactSlot{Dir: reportDir, Stamp: stamp}
		if thumbnailDir != "" {
			dayDir, err := safeFilePath(thumbnailDir, stamp)
			if err != nil {
				return ArtifactSlot{}, err
			}
			slot.DayDir = dayDir
		}
		free, err := slotFree(slot)
		if err != nil {
			return ArtifactSlot{}, err
		}
		if free {
			return slot, nil
		}
	}
	return ArtifactSlot{}, fmt.Errorf("there are already %d sets of artifacts of %s in %s; this run writes none",
		maxRunsPerDay, base, reportDir)
}

// slotFree reports whether none of the four names is already in use. Lstat and
// not Stat: a symlink with tomorrow's name may not count as "free", and we
// certainly do not want to follow it.
func slotFree(a ArtifactSlot) (bool, error) {
	paths := make([]string, 0, 4)
	for _, name := range []string{a.ReportName(), a.ScriptName(), a.ListName()} {
		path, err := safeFilePath(a.Dir, name)
		if err != nil {
			return false, err
		}
		paths = append(paths, path)
	}
	if a.DayDir != "" {
		paths = append(paths, a.DayDir)
	}
	for _, path := range paths {
		_, err := os.Lstat(path)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			// Do not silently move on to the next stamp: if we cannot determine
			// *what* is there, we also do not know whether we would overwrite it.
			// Then rather no artifacts and a side issue.
			return false, fmt.Errorf("cannot determine whether %s already exists: %w", path, err)
		}
	}
	return true, nil
}

// WriteReport puts down the list file of this run and returns the path. The
// file name comes from the ArtifactSlot of the run (g.Name) and is not derived
// from the date here — see the explanation at ArtifactSlot for why a second run
// on the same day may not overwrite the report of the first.
func WriteReport(log *slog.Logger, dir string, g ReportData) (string, error) {
	if g.Name == "" {
		return "", errors.New("report without a file name; the ArtifactSlot of this run was not passed on")
	}
	path, err := safeFilePath(dir, g.Name)
	if err != nil {
		return "", err
	}
	// makeDirWithGroup and not os.MkdirAll: the latter gives every directory it
	// creates the group of the process (root), and then there is a report
	// directory as root:root that the admin cannot enter — exactly where the
	// report from the notification ends up. Also applies to the dry-run
	// directory under REPORT_DIR.
	if err := makeDirWithGroup(log, dir); err != nil {
		return "", fmt.Errorf("report directory %s cannot be created: %w", dir, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "photowatch — deleted files on %s\n", g.Now.Format("2006-01-02 15:04:05 -0700"))
	if g.DryRun {
		// Write down exactly what a dry run does and does not do. The report, the
		// restore script, the list and the thumbnails are written (so that you
		// can check whether they are right), but only in the separate dry-run
		// directory this file is in now: outside that directory a dry run
		// creates, changes or deletes nothing, and nothing goes to Home
		// Assistant.
		b.WriteString("NOTE: dry run. No snapshot was made, nothing was created, changed or\n" +
			"deleted outside this dry-run directory, and nothing was reported. The\n" +
			"report, the restore script, the list and the thumbnails of this dry run\n" +
			"sit next to each other in the dry-run directory; the next dry run\n" +
			"replaces them.\n")
	}
	b.WriteString(strings.Repeat("=", 72) + "\n\n")
	// The dataset passed datasetRegexp and can therefore hold nothing odd; the
	// path prefix (from the environment) and the snapshot name (from `zfs list`)
	// were only checked for shape and therefore go through pathText, like the
	// paths further down. A report is a file somebody runs `cat` on.
	fmt.Fprintf(&b, "Dataset:            %s\n", g.Dataset)
	if g.PathPrefix != "" {
		fmt.Fprintf(&b, "Path prefix:        %s (only below this was counted)\n", pathText(g.PathPrefix))
	}
	fmt.Fprintf(&b, "Previous snapshot:  %s\n", pathText(g.PreviousSnapshot))
	fmt.Fprintf(&b, "Created at:         %s\n", g.PreviousTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "New snapshot:       %s\n", g.NewSnapshot)
	fmt.Fprintf(&b, "Period:             %s\n\n", durationInWords(g.Now.Sub(g.PreviousTime)))

	fmt.Fprintf(&b, "Deleted files:      %d   (%s, %s; threshold %d, alert: %s)\n",
		len(g.Diff.Deleted),
		plural(g.Diff.MediaFiles(), "1 media file", "%d media files"),
		plural(g.Diff.SidecarFiles(), "1 sidecar file", "%d sidecar files"),
		g.Threshold, yesNo(g.Alert))
	if !g.RecoverUntil.IsZero() {
		fmt.Fprintf(&b, "Recoverable until:  %s   (as long as %s exists)\n",
			g.RecoverUntil.Format("2006-01-02"), pathText(g.PreviousSnapshot))
	}
	switch {
	case g.Restore.ScriptPath != "":
		fmt.Fprintf(&b, "Restore script:     %s   (%d files)\n", g.Restore.ScriptPath, g.Restore.Lines)
	case g.Restore.Message != "":
		fmt.Fprintf(&b, "Restore script:     not written — %s\n", cleanText(g.Restore.Message, 300))
	}
	switch {
	case g.Thumbnail.Made > 0:
		fmt.Fprintf(&b, "Thumbnails:         %s   (%d of %d suitable photos)\n",
			g.Thumbnail.DayDir, g.Thumbnail.Made, g.Thumbnail.Candidates)
	case g.Thumbnail.Message != "":
		fmt.Fprintf(&b, "Thumbnails:         none — %s\n", cleanText(g.Thumbnail.Message, 300))
	case g.Thumbnail.Candidates > 0:
		// There were suitable photos, but not one came out and there is no
		// message explaining that either (the day directory could not be created,
		// for instance). Without this line the report stays silent about it.
		fmt.Fprintf(&b, "Thumbnails:         none — not one of the %d suitable photos succeeded\n",
			g.Thumbnail.Candidates)
	case len(g.Thumbnail.Reasons) > 0:
		// Judged, but nothing was suitable: everything that disappeared was
		// video, raw or a sidecar. That is the day on which this line and the
		// list below it are all there is to say about the thumbnails — hence the
		// plan travels along even when nothing is scaled.
		b.WriteString("Thumbnails:         none — none of the deleted files is a jpg/png/gif\n")
	}
	fmt.Fprintf(&b, "Deleted directories: %d\n", g.Diff.DeletedDirs)
	fmt.Fprintf(&b, "Deleted other:      %d   (symlinks and such; no alert)\n", g.Diff.DeletedOther)
	fmt.Fprintf(&b, "Renamed:            %d   (does not count as deleted)\n", g.Diff.Renamed)
	fmt.Fprintf(&b, "Added:              %d\n", g.Diff.Added)
	fmt.Fprintf(&b, "Modified:           %d\n", g.Diff.Modified)
	if g.Diff.Skipped > 0 {
		fmt.Fprintf(&b, "Outside the prefix: %d\n", g.Diff.Skipped)
	}
	if g.Diff.Unparsed > 0 {
		fmt.Fprintf(&b, "Unparsed lines:     %d   (output of zfs diff that did not match the expected shape)\n", g.Diff.Unparsed)
	}
	if g.Diff.NotDecodable > 0 {
		fmt.Fprintf(&b, "Not decodable:      %d   (listed below with a ! in front, in raw form)\n", g.Diff.NotDecodable)
	}
	b.WriteString("\n")

	b.WriteString(restoreInstructions(g))

	if len(g.Thumbnail.Reasons) > 0 {
		b.WriteString("Why some files have no thumbnail:\n")
		for _, r := range sortedReasons(g.Thumbnail.Reasons) {
			fmt.Fprintf(&b, "  %-38s %d\n", r.reason, r.count)
		}
		b.WriteString("\n")
	}

	b.WriteString("Deleted files, one path per line:\n")
	b.WriteString("A [nnn] in front refers to the thumbnail with that number; a ! means the\n")
	b.WriteString("path could not be decoded and is shown here in raw form.\n")
	b.WriteString(strings.Repeat("-", 72) + "\n")
	shown := g.Diff.Deleted
	truncated := 0
	if len(shown) > maxReportLines {
		truncated = len(shown) - maxReportLines
		shown = shown[:maxReportLines]
	}
	for _, v := range shown {
		switch {
		case !v.Decodable:
			// The raw form goes through pathText as well. It used to say that was
			// unnecessary because zfs diff writes everything below space
			// octally — but this branch is reached exactly when the round trip
			// failed, so precisely when the output was *not* in the expected
			// format. Raw is the third tab field of a line and can therefore hold
			// any byte except tab and newline. Without this filtering an ESC in a
			// path would perform cursor movements during `cat deleted-*.txt`, in
			// exactly the file the reader has to decide from whether to restore. A
			// correctly escaped path is unaffected.
			fmt.Fprintf(&b, "    ! %s   (not decodable; raw form)\n", pathText(v.Raw))
		case g.Thumbnail.Numbers[v.Raw] > 0:
			fmt.Fprintf(&b, "[%03d] %s\n", g.Thumbnail.Numbers[v.Raw], pathText(v.Path))
		default:
			fmt.Fprintf(&b, "      %s\n", pathText(v.Path))
		}
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "... and %d more lines, truncated here at %d.\n"+
			"The full list can be regenerated as long as the snapshot exists:\n"+
			"  zfs diff -H -F %s %s\n", truncated, maxReportLines, pathText(g.PreviousSnapshot), g.Dataset)
	}

	content := []byte(b.String())
	if err := writeAtomic(log, path, modeReport, content); err != nil {
		return "", err
	}

	// The copy on the share sits next to the thumbnails: open that directory in
	// a file manager and you see the images with the report beside them. If the
	// copy fails, that is a WARN and not a failure — the real report is there.
	//
	// The restore script deliberately does *not* travel to the share: an
	// executable script that root is going to run may not sit in a directory
	// others can write to.
	if g.CopyDir != "" {
		copyPath, err := safeFilePath(g.CopyDir, "report.txt")
		if err != nil {
			log.Warn("copy of the report cannot be placed", "dir", g.CopyDir, "error", err)
		} else if err := makeDirWithGroup(log, g.CopyDir); err != nil {
			log.Warn("day directory for the copy of the report cannot be created", "dir", g.CopyDir, "error", err)
		} else if err := writeAtomic(log, copyPath, modeReport, content); err != nil {
			log.Warn("copy of the report cannot be written; the original is there",
				"copy", copyPath, "original", path, "error", err)
		} else {
			log.Debug("copy of the report placed on the share", "path", copyPath)
		}
	}
	return path, nil
}

// reasonLine is one line of the "why no thumbnail" list.
type reasonLine struct {
	reason string
	count  int
}

// sortedReasons makes the list stable: a map has no order and a report that
// comes in a different order every day reads badly.
func sortedReasons(m map[string]int) []reasonLine {
	out := make([]reasonLine, 0, len(m))
	for reason, count := range m {
		out = append(out, reasonLine{reason: reason, count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].reason < out[j].reason })
	return out
}

// pathText makes a decoded path suitable to show as text. Unlike cleanText it
// does *not* trim spaces from the edges: a file name that starts with a space
// exists, and in a report somebody copies a path from, that may not disappear
// silently. Control characters should not be in there anyway (they make a path
// non-decodable), but this function is the second safeguard on that rule.
func pathText(s string) string {
	return shortText(stripControlChars(s), maxPathText)
}

// restoreInstructions explains how to restore: first the ready-made script,
// only then the manual rsync line as a fallback.
//
// Restoring happens by hand, but not from memory. Without arguments the script
// does nothing but show, it can only create files and never change or delete
// them, and it refuses to run when the snapshot is gone.
func restoreInstructions(g ReportData) string {
	var b strings.Builder
	b.WriteString("Restoring\n")
	b.WriteString(strings.Repeat("-", 72) + "\n")
	if !g.RecoverUntil.IsZero() {
		fmt.Fprintf(&b, "This is possible through %s. After that the snapshot disappears\n"+
			"and it is gone.\n\n", g.RecoverUntil.Format("2006-01-02"))
	}

	if g.Restore.ScriptPath != "" {
		b.WriteString("On the host that holds the ZFS pool, as root:\n\n")
		fmt.Fprintf(&b, "  bash %s            # shows what would happen\n", g.Restore.ScriptPath)
		fmt.Fprintf(&b, "  bash %s --apply    # really restores it\n\n", g.Restore.ScriptPath)
		b.WriteString("The script can only CREATE files: what is there now is never\n" +
			"overwritten and never deleted. To get only part of it back, remove\n" +
			"lines from the matching .list file (NUL separated).\n" +
			"Do not move the script to the share: it runs as root and belongs in a\n" +
			"directory only root can write to.\n\n")
	} else if g.Restore.Message != "" {
		fmt.Fprintf(&b, "NOTE: %s\n", cleanText(g.Restore.Message, 300))
		b.WriteString("Use the example below by hand instead.\n\n")
	}

	b.WriteString("By hand, from the snapshot taken before the deletion. It sits\n")
	b.WriteString("read-only under .zfs/snapshot/:\n\n")

	exampleSource := "<mountpoint>/.zfs/snapshot/" + g.PreviousShort + "/<path inside the dataset>"
	exampleTarget := "<mountpoint>/<path inside the dataset>"
	if g.Mountpoint != "" {
		if v, ok := firstDecodable(g.Diff.Deleted); ok {
			if rel, ok := relativeWithin(g.Mountpoint, v.Path); ok {
				exampleSource = filepath.Join(SnapshotDirFor(g.Mountpoint, g.PreviousShort), rel)
				exampleTarget = v.Path
			}
		}
	}
	fmt.Fprintf(&b, "  rsync -aHAX --info=progress2 \\\n    '%s' \\\n    '%s'\n\n",
		pathText(exampleSource), pathText(exampleTarget))
	b.WriteString("A whole directory at once (note the trailing /):\n\n")
	if g.Mountpoint != "" {
		// The mountpoint and the snapshot name go through pathText too: they come
		// from `zfs get` and `zfs list` and therefore from outside this program.
		// They should hold no control characters, and this is the safeguard that
		// enforces it at the place where they land as text in a file somebody
		// runs `cat` on.
		fmt.Fprintf(&b, "  rsync -aHAX --ignore-existing '%s/<dir>/' '%s/<dir>/'\n\n",
			pathText(SnapshotDirFor(g.Mountpoint, g.PreviousShort)), pathText(g.Mountpoint))
	} else {
		fmt.Fprintf(&b, "  rsync -aHAX --ignore-existing '<mountpoint>/.zfs/snapshot/%s/<dir>/' '<mountpoint>/<dir>/'\n\n", pathText(g.PreviousShort))
	}
	b.WriteString("If a path below holds a single quote or a space, put it between quotes\n" +
		"yourself. Paths with a ! in front could not be decoded and are shown in\n" +
		"the raw form of `zfs diff`, with octal escapes (\\0040 is a space, \\0134\n" +
		"a backslash); convert those by hand and check with `ls` before copying.\n\n")
	// The library scan is a nudge and not a precondition: Immich sees a restored
	// file right away. docs/RECOVERY.md leads on this.
	b.WriteString("Then check in Immich whether the photos are back; usually Immich sees\n" +
		"them right away. If not, Administration > Libraries > Scan all is the\n" +
		"nudge.\n\n")
	return b.String()
}

// firstDecodable finds a path that can serve as an example. A path that cannot
// be decoded would be exactly the path that does *not* work if you copy it.
func firstDecodable(list []DeletedFile) (DeletedFile, bool) {
	for _, v := range list {
		if v.Decodable {
			return v, true
		}
	}
	return DeletedFile{}, false
}

// relativeWithin returns the path relative to the mountpoint, and whether the
// path falls inside it at all. Without that second check a path from outside
// the dataset would silently produce a nonsensical restore example.
func relativeWithin(mountpoint, path string) (string, bool) {
	mp := filepath.Clean(mountpoint)
	p := filepath.Clean(path)
	if mp == "/" {
		return strings.TrimPrefix(p, "/"), true
	}
	if !strings.HasPrefix(p, mp+"/") {
		return "", false
	}
	return strings.TrimPrefix(p, mp+"/"), true
}

// StateFile is what goes into status.json: the payload of the last run, plus
// three things that do not go to Home Assistant but are needed on the host.
// Reported and Invocation exist for the OnFailure unit: it has to be able to
// see that the run already reported itself, otherwise two messages arrive for
// one failure.
//
// Note what Reported does and does not say: Home Assistant accepted the message
// (a 2xx). That is not the same as "a notification appeared on the phone" — HA
// also answers 200 to an unknown webhook id and to a request it refuses because
// of local_only. There is no more to say about it from this side; the dead
// man's switch in HA exists for that case.
type StateFile struct {
	Written  string `json:"written"`  // RFC3339; when this run finished
	Reported bool   `json:"reported"` // did HA accept the message (2xx)?
	// Invocation is systemd's $INVOCATION_ID: an id unique per start of the
	// unit. The OnFailure unit compares it with $MONITOR_INVOCATION_ID and thus
	// knows whether this state file is about exactly the run that just fell
	// over. Empty when the program was started outside systemd.
	Invocation string  `json:"invocation,omitempty"`
	Payload    Payload `json:"payload"`
	// Aftercare is what the work after the notification produced: the
	// thumbnails, the report and the cleanup of artifacts. That work only starts
	// once the payload has been sent, so it *cannot* be in that message any
	// more. It is here, and the *next* run puts SideIssues at the front of its
	// own side_issues field — that way a failure that persists still reaches the
	// phone, one day later. That is the price of the order that gives the alert
	// precedence over the comfort.
	Aftercare *AftercareResult `json:"aftercare,omitempty"`
}

// AftercareResult is the outcome of the work after the notification.
type AftercareResult struct {
	Thumbnails int `json:"thumbnails"`
	CleanedUp  int `json:"cleaned_up"`
	// ArtifactsMB is the size of the report directory plus the thumbnail
	// directory, measured after the cleanup of this run. It is here and not in
	// the payload of the same run for two reasons: measuring is a walk over
	// THUMBNAIL_DIR and that may not happen before the notification (if that
	// directory ever points at a network mount, a hung mount holds up the
	// alert), and after the cleanup it is a more honest number. The *next* run
	// puts it in its payload.
	ArtifactsMB int    `json:"artifacts_mb"`
	SideIssues  string `json:"side_issues,omitempty"`
}

// WriteState records the last run in status.json. That is the third layer of
// monitoring next to the HA sensor and the "task is silent" automation: whoever
// is on the host sees here without detours when the watch last ran.
func WriteState(log *slog.Logger, dir string, s StateFile) (string, error) {
	path, err := safeFilePath(dir, "status.json")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, modeDir); err != nil {
		return "", fmt.Errorf("state directory %s cannot be created: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", fmt.Errorf("state cannot be encoded as JSON: %w", err)
	}
	data = append(data, '\n')
	if err := writeAtomic(log, path, modeReport, data); err != nil {
		return "", err
	}
	return path, nil
}

// ReadState fetches the state file of the previous run. If it is missing, that
// is not an error but information: this photowatch has then never finished a
// run on this host. Hence the bool instead of os.IsNotExist at every caller.
func ReadState(dir string) (StateFile, bool, error) {
	path, err := safeFilePath(dir, "status.json")
	if err != nil {
		return StateFile{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return StateFile{}, false, nil
	}
	if err != nil {
		return StateFile{}, false, fmt.Errorf("state file %s not readable: %w", path, err)
	}
	var s StateFile
	if err := json.Unmarshal(data, &s); err != nil {
		return StateFile{}, false, fmt.Errorf("state file %s does not hold valid JSON: %w", path, err)
	}
	return s, true, nil
}

// writeAtomic first writes to a temporary file in the same directory and
// renames it afterwards. That way a concurrent reader never sees half a file:
// whoever runs `photowatch -status` during the run sees the old or the new
// version, not something in between.
//
// For a power cut that only half holds, and that is a deliberate boundary. The
// contents are pushed to disk with Sync before the rename, but the directory
// itself is not synced. If the host dies exactly in between, the rename can be
// lost and the old file is still there — annoying, but the safe side: run()
// only reads status.json to determine whether it ran before, and an old value
// leads at most to one notification too many. An empty or truncated status.json
// *would* do harm, and that is what the Sync prevents.
func writeAtomic(log *slog.Logger, path string, mode os.FileMode, data []byte) error {
	dir := filepath.Dir(path)
	removeStaleTemps(log, dir, time.Now())
	f, err := os.CreateTemp(dir, ".photowatch-*")
	if err != nil {
		return fmt.Errorf("could not create a temporary file in %s: %w", dir, err)
	}
	temp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing to %s failed: %w", temp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("flushing %s to disk failed: %w", temp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s failed: %w", temp, err)
	}
	// CreateTemp makes 600; set the mode explicitly and do not rely on the
	// umask, otherwise the admin group cannot read the report.
	if err := os.Chmod(temp, mode); err != nil {
		return fmt.Errorf("permissions of %s cannot be set: %w", temp, err)
	}
	if err := setGroup(dir, temp); err != nil {
		// No reason to throw away an otherwise fully written report. This fails
		// when the group of the directory does not exist, or when somebody runs
		// it by hand as a user with their own REPORT_DIR whose group they are not
		// in: the only damage is then that one user cannot read the file. Log it
		// loudly, because after this the report the notification points at can no
		// longer be opened without sudo.
		log.Warn("group of the new file cannot be set; the file is written anyway",
			"path", path, "error", err,
			"consequence", "possibly readable by root only; check the owner and group of the directory")
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("renaming to %s failed: %w", path, err)
	}
	cleanup = false
	return nil
}

// setGroup is a variable and not a direct call, so that the test can imitate
// the failure path. A failed chown may not throw away an otherwise fully
// written report, and that cannot be forced as an ordinary user: chowning to a
// group you are not in requires root, and as root it never fails.
var setGroup = groupFromDir

// groupFromDir gives the new file the same group as the directory it goes into.
// Without this there would be root:root 0640 in the report directory and the
// admin cannot open the reports the notification points at: a new file gets the
// group of the process (root), not that of the directory.
//
// The group name is deliberately nowhere in this code. Installation sets it on
// the directory, and whoever wants to change it does so there — one chown, no
// recompile.
//
// The alternative was the setgid bit on the directory, which lets the kernel do
// this. That is one line less code but then it only lives in the install
// instructions: whoever ever recreates the directory by hand loses it and
// notices nothing. Hence here, in the code that writes the file.
func groupFromDir(dir, file string) error {
	di, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("directory %s cannot be stat'ed: %w", dir, err)
	}
	dstat, ok := di.Sys().(*syscall.Stat_t)
	if !ok {
		// On a system without unix ownership there is nothing to set. Not an
		// error: the file is there and readable for whoever made it.
		return nil
	}
	fi, err := os.Stat(file)
	if err != nil {
		return fmt.Errorf("temporary file %s cannot be stat'ed: %w", file, err)
	}
	fstat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fstat.Gid == dstat.Gid {
		return nil
	}
	// -1 for the owner: we leave that as it is.
	if err := os.Chown(file, -1, int(dstat.Gid)); err != nil {
		return fmt.Errorf("group %d (of directory %s) cannot be set on %s: %w", dstat.Gid, dir, file, err)
	}
	return nil
}

// makeDirWithGroup creates a directory with mode 750 and gives it the group of
// its parent — and does that for *every* level it has to create itself.
//
// Setting the group is needed for the same reason as for a new file, but it is
// easier to overlook: os.MkdirAll gives a new directory the group of the
// process (root), not that of the directory above it. Without the explicit
// Chown the day directory is root:root and cannot be opened over Samba — while
// that was exactly the requirement that put the thumbnails on a share.
//
// This used to be one MkdirAll followed by Chmod and Chown on only the deepest
// path. The same trap was then still one level up: if the parent directory did
// not exist yet (somebody sets THUMBNAIL_DIR to a nested path without creating
// the parent by hand), MkdirAll created *that* as root:root, setGroup then
// inherited that fresh root group from the parent, "succeeded" and stayed
// silent. With `hide unreadable = yes` in Samba you then see no error at all but
// simply nothing — while the notification does point at that path.
//
// Hence the recursion: every level we create gets the same treatment. A level
// that was already there we leave alone — mode *and* group. That is not only
// modesty: REPORT_DIR is set by hand during installation to root:<group> 750,
// and if this function put the group of the parent directory (/var/log, so
// root) over it, it would remove exactly the setting that makes the reports
// readable.
//
// A failed chown is not an error here but a loud WARN, for the same reason as
// in writeAtomic: the directory is there and the work can continue; the only
// damage is that one user cannot enter it. That also keeps the promise that a
// chown problem never costs an otherwise fully written report.
func makeDirWithGroup(log *slog.Logger, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("directory %q is not an absolute path", path)
	}
	clean := filepath.Clean(path)
	parent := filepath.Dir(clean)
	// parent == clean means: we are at "/". That always exists; there is nothing
	// above it.
	if parent != clean {
		switch _, err := os.Lstat(parent); {
		case err == nil:
			// The parent directory is already there: do not touch it.
		case errors.Is(err, os.ErrNotExist):
			if err := makeDirWithGroup(log, parent); err != nil {
				return err
			}
		default:
			return fmt.Errorf("parent directory %s cannot be stat'ed: %w", parent, err)
		}
	}
	if err := os.Mkdir(clean, modeDir); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("directory %s cannot be created: %w", clean, err)
		}
		// Something with this name is already there. Lstat and not Stat: if it is
		// a symlink, a Chmod or Chown would *follow* it and set the permissions
		// of something entirely different — possibly outside the intended
		// directory. MkdirAll used to let that pass silently.
		fi, err := os.Lstat(clean)
		if err != nil {
			return fmt.Errorf("directory %s cannot be stat'ed: %w", clean, err)
		}
		if !fi.Mode().IsDir() {
			return fmt.Errorf("%s already exists and is not an ordinary directory (%s)", clean, fi.Mode().Type())
		}
		// It was already there: mode and group are not ours.
		return nil
	}
	// Mkdir respects the umask; set the mode explicitly, otherwise it is
	// 0750 & ~umask and the group cannot enter the directory.
	if err := os.Chmod(clean, modeDir); err != nil {
		return fmt.Errorf("permissions of %s cannot be set: %w", clean, err)
	}
	if err := setGroup(parent, clean); err != nil {
		log.Warn("group of the new directory cannot be set; the directory is used anyway",
			"dir", clean, "error", err,
			"consequence", "possibly openable by root only; over Samba it is then invisible",
			"check", "the owner and group of "+parent+" (ls -ld)")
	}
	return nil
}

// safeFilePath joins directory and name and checks that the result stays inside
// the directory. The names come from the code here, but the directory comes
// from the configuration: this is the cheap check that keeps a wrongly filled
// in REPORT_DIR from writing somewhere else.
func safeFilePath(dir, name string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("directory %q is not an absolute path", dir)
	}
	clean := filepath.Clean(dir)
	path := filepath.Join(clean, name)
	if filepath.Dir(path) != clean {
		return "", fmt.Errorf("file name %q would end up outside %s", name, clean)
	}
	return path, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func durationInWords(d time.Duration) string {
	if d < 0 {
		return fmt.Sprintf("%s (negative: the clock of the host was changed)", d.Round(time.Second))
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	return fmt.Sprintf("%d hours %d minutes", hours, minutes)
}
