package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// All file deletion of this program lives in this file — just as all snapshot
// deletion lives in retention.go. The value of that is that a reader can check
// in one place what may ever be thrown away.
//
// os.RemoveAll does not occur in this project. That is the equivalent of `zfs
// destroy -r`, and the same rule applies: never. A day directory is descended
// into by hand, one level, file by file.

// The guardrails on cleaning up artifacts, with the same layout as the three in
// retention.go.
const (
	// Guardrail 6: how many always stay, no matter how old. If the system clock
	// jumps forward, everything suddenly looks too old.
	MinimumReports = 14
	MinimumRestore = 3
	MinimumDayDirs = 3

	// Guardrail 7: how many items may go per run at most. A day directory counts
	// as one. With a larger backlog the rest follows on the days after, with a
	// WARN — that way one wrong assumption can never cost everything at once.
	MaxCleanupPerRun = 20

	// The margin on top of KEEP_DAYS for the artifacts that belong to a
	// snapshot. A restore script or a thumbnail without its snapshot has no
	// purpose left; two days of slack so that an artifact never disappears
	// before its snapshot.
	ArtifactDayMargin = 2
)

// Guardrail 3: only names that match our own pattern exactly. Anything else
// stays, no matter how old. Whoever renames a report to
// deleted-2026-08-31-IMPORTANT.txt has kept it forever, and that is exactly the
// intention.
//
// The optional -<digits> after the date is the stamp of the second and later
// run of a day (see ArtifactSlot in report.go). Two digits and no more, because
// maxRunsPerDay is 99; that keeps the pattern as narrow as it was and nothing
// matches that we could not have written ourselves. The date for the retention
// is and stays group 1.
var (
	reportName        = regexp.MustCompile(`^deleted-(\d{4}-\d{2}-\d{2})(?:-\d{1,2})?\.txt$`)
	restoreScriptName = regexp.MustCompile(`^restore-(\d{4}-\d{2}-\d{2})(?:-\d{1,2})?\.sh$`)
	restoreListName   = regexp.MustCompile(`^restore-(\d{4}-\d{2}-\d{2})(?:-\d{1,2})?\.list$`)
	dayDirName        = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:-\d{1,2})?$`)
	dayDirReport      = regexp.MustCompile(`^report\.txt$`)
)

// Directories that may never be a REPORT_DIR or THUMBNAIL_DIR. Not because the
// name check would not work there, but because a typo in the configuration has
// the most expensive consequences here: those directories hold files of others
// that might happen to match our pattern.
var forbiddenCleanupDirs = []string{"/", "/var", "/var/log", "/var/lib", "/mnt", "/etc", "/usr", "/home", "/root", "/tmp"}

// CleanupResult is what went away this run, per kind.
type CleanupResult struct {
	Reports int
	Scripts int
	Lists   int
	DayDirs int
	Backlog bool
	Message string // short text for the payload when something could not be done
}

// Total is the number of deleted items; a day directory counts as one.
func (o CleanupResult) Total() int {
	return o.Reports + o.Scripts + o.Lists + o.DayDirs
}

// CleanupJob is what CleanArtifacts needs to know. The mountpoint is a separate
// field and not part of Config because it is only known during the run (it
// comes from `zfs get mountpoint`) — and that path in particular may never be a
// target directory.
type CleanupJob struct {
	ReportDir        string
	ThumbnailDir     string // empty = no thumbnails
	StateDir         string
	Mountpoint       string // empty when it could not be determined
	KeepDaysReport   int
	ArtifactKeepDays int
	DryRun           bool
}

// CleanArtifacts removes old reports, restore scripts, lists and day
// directories with thumbnails. It runs on *every* run, also on a day without
// deletions — otherwise the pile on the hypervisor simply keeps growing as soon
// as nothing disappears for a while.
func CleanArtifacts(log *slog.Logger, o CleanupJob, now time.Time) (CleanupResult, error) {
	var res CleanupResult
	// Every failure is kept, not overwritten: four kinds can go wrong at once
	// and there is one field in the payload. With `res.Message = ...` only the
	// *last* one would be visible — for example a day directory that stays,
	// while the more serious "report directory cannot be walked" happened
	// earlier in the same run.
	var messages []string

	if err := checkCleanupDir("REPORT_DIR", o.ReportDir, o); err != nil {
		return res, err
	}
	// The most important check on THUMBNAIL_DIR is "does not lie in or on the
	// mountpoint of the watched dataset". That cannot be done when the
	// mountpoint is unknown (`zfs get mountpoint` failed, or the dataset is on
	// legacy/none). We then do not clean that directory this run instead of
	// continuing without the safeguard: with a misconfigured THUMBNAIL_DIR the
	// cleanup would otherwise descend into the photo archive, and an empty day
	// directory named YYYY-MM-DD *would* disappear there.
	//
	// REPORT_DIR above does continue with an unknown mountpoint, and that is a
	// deliberate difference: it only holds files photowatch writes itself
	// (deleted-*.txt, restore-*.sh, restore-*.list), and stopping everything
	// would mean that a hiccup in `zfs get` lets the reports pile up forever.
	cleanThumbnails := o.ThumbnailDir != ""
	if cleanThumbnails && o.Mountpoint == "" {
		cleanThumbnails = false
		addCleanupMessage(&messages, "thumbnail directory not cleaned: the mountpoint of the dataset is unknown")
		log.Warn("thumbnail directory not cleaned this run because the mountpoint of the dataset is unknown",
			"dir", o.ThumbnailDir,
			"consequence", "the check 'THUMBNAIL_DIR is not inside the watched archive' cannot be done; nothing is deleted there",
			"check", "`zfs get mountpoint <dataset>` on the host")
	}
	if cleanThumbnails {
		if err := checkCleanupDir("THUMBNAIL_DIR", o.ThumbnailDir, o); err != nil {
			return res, err
		}
	}

	budget := MaxCleanupPerRun
	backlog := false

	kinds := []struct {
		what     string
		dir      string
		pattern  *regexp.Regexp
		keepDays int
		minimum  int
		counter  *int
	}{
		{"report", o.ReportDir, reportName, o.KeepDaysReport, MinimumReports, &res.Reports},
		{"restore script", o.ReportDir, restoreScriptName, o.ArtifactKeepDays, MinimumRestore, &res.Scripts},
		// A list without a script is useless and a script without a list is
		// broken: hence the same period and the same minimum as the script.
		{"restore list", o.ReportDir, restoreListName, o.ArtifactKeepDays, MinimumRestore, &res.Lists},
	}

	for _, s := range kinds {
		candidates, err := oldItems(log, s.dir, s.pattern, s.keepDays, s.minimum, now, false)
		if err != nil {
			log.Warn("directory cannot be walked to clean up; the run continues",
				"kind", s.what, "dir", s.dir, "error", err)
			addCleanupMessage(&messages, "cleanup failed: "+s.dir+" cannot be walked")
			continue
		}
		for _, k := range candidates {
			if budget <= 0 {
				backlog = true
				break
			}
			if o.DryRun {
				log.Info("dry run: would delete", "kind", s.what, "path", k.path)
				budget--
				*s.counter++
				continue
			}
			if err := os.Remove(k.path); err != nil {
				log.Warn("old artifact cannot be deleted; the run continues",
					"kind", s.what, "path", k.path, "error", err)
				addCleanupMessage(&messages, "cleanup failed: "+k.path+" cannot be deleted")
				continue
			}
			budget--
			*s.counter++
			log.Debug("old artifact deleted", "kind", s.what, "path", k.path, "date", k.date.Format("2006-01-02"))
		}
	}

	if cleanThumbnails {
		candidates, err := oldItems(log, o.ThumbnailDir, dayDirName, o.ArtifactKeepDays, MinimumDayDirs, now, true)
		if err != nil {
			log.Warn("thumbnail directory cannot be walked to clean up; the run continues",
				"dir", o.ThumbnailDir, "error", err)
			addCleanupMessage(&messages, "cleanup failed: "+o.ThumbnailDir+" cannot be walked")
		}
		for _, k := range candidates {
			if budget <= 0 {
				backlog = true
				break
			}
			gone, err := removeDayDir(log, k.path, o.DryRun)
			if err != nil {
				log.Warn("day directory with thumbnails cannot be (fully) cleaned up; it stays",
					"dir", k.path, "error", err)
				addCleanupMessage(&messages, "cleanup failed: day directory "+k.path+" stays")
				continue
			}
			if !gone {
				continue // a human put something there; removeDayDir already warned
			}
			budget--
			res.DayDirs++
		}
	}

	res.Backlog = backlog
	res.Message = strings.Join(messages, "; ")
	if backlog {
		log.Warn("more old artifacts than may go per run; the rest follows in the coming days",
			"max_per_run", MaxCleanupPerRun, "report_dir", o.ReportDir, "thumbnail_dir", o.ThumbnailDir)
	}
	if res.Total() > 0 {
		log.Info("files cleaned up",
			"reports", res.Reports, "restore_scripts", res.Scripts, "restore_lists", res.Lists,
			"day_dirs", res.DayDirs, "dry_run", o.DryRun)
	} else {
		// DEBUG and not INFO when nothing was cleaned: otherwise half the
		// journal is noise about a run in which nothing happened.
		log.Debug("nothing to clean up", "report_dir", o.ReportDir, "thumbnail_dir", o.ThumbnailDir)
	}
	return res, nil
}

// checkCleanupDir is the validation before anything at all is deleted.
func checkCleanupDir(key, dir string, o CleanupJob) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s %q is not an absolute path; nothing is cleaned up", key, dir)
	}
	clean := filepath.Clean(dir)
	for _, forbidden := range forbiddenCleanupDirs {
		if clean == forbidden {
			return fmt.Errorf("%s is set to %q; photowatch never cleans up there, because that holds files of others", key, clean)
		}
	}
	if o.StateDir != "" && clean == filepath.Clean(o.StateDir) {
		return fmt.Errorf("%s equals STATE_DIR (%s); that holds status.json and that is not an artifact", key, clean)
	}
	// Note the condition: when the mountpoint is unknown, nothing is checked
	// here. For THUMBNAIL_DIR the caller may not lean on that — it then skips
	// the cleanup entirely (see CleanArtifacts). This line is therefore not the
	// safeguard but its execution once we know.
	if o.Mountpoint != "" && withinPathPrefix(filepath.Clean(o.Mountpoint), clean) {
		return fmt.Errorf("%s (%s) lies in or on the mountpoint %s of the watched dataset; nothing is ever deleted there", key, clean, o.Mountpoint)
	}
	return nil
}

// maxCleanupMessages is how many different failures fit in one payload. The
// field ends up on a phone screen and is cut off there anyway; whoever wants to
// see more looks in the journal. Four is enough to see *that* more than one
// kind went wrong.
const maxCleanupMessages = 4

// addCleanupMessage adds one failure, without repetition. Twenty times "cannot
// be deleted" is one problem, not twenty.
func addCleanupMessage(list *[]string, text string) {
	for _, existing := range *list {
		if existing == text {
			return
		}
	}
	if len(*list) == maxCleanupMessages {
		*list = append(*list, "and more; see the journal")
		return
	}
	if len(*list) > maxCleanupMessages {
		return
	}
	*list = append(*list, text)
}

// cleanupItem is one candidate: the full path and the date that was in the
// name.
type cleanupItem struct {
	path string
	date time.Time
}

// oldItems applies the guardrails and returns what may go, oldest first.
//
// Guardrail 5: the age comes from the name and not from mtime. An mtime changes
// on copying and on a touch; the date in the name is what we put there
// ourselves. The date must also be a real date and not lie in the future —
// otherwise a file named deleted-9999-01-01.txt would send the counts haywire.
func oldItems(log *slog.Logger, dir string, pattern *regexp.Regexp, keepDays, minimum int, now time.Time, dirs bool) ([]cleanupItem, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing has ever been written; nothing wrong.
			return nil, nil
		}
		return nil, err
	}

	var own []cleanupItem
	for _, item := range items {
		match := pattern.FindStringSubmatch(item.Name())
		if match == nil {
			continue
		}
		// Guardrail: only regular files and real directories, never follow a
		// symlink. DirEntry.Type() comes from lstat, so a symlink that looks
		// like a day directory is ModeSymlink here and drops out.
		if dirs && !item.IsDir() {
			log.Debug("name matches a day directory but it is not a directory; skipped", "name", item.Name(), "dir", dir)
			continue
		}
		if !dirs && !item.Type().IsRegular() {
			log.Debug("name matches an artifact but it is not a regular file; skipped", "name", item.Name(), "dir", dir)
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", match[1], now.Location())
		if err != nil {
			log.Debug("name holds no valid date; it stays", "name", item.Name(), "dir", dir)
			continue
		}
		if date.After(now) {
			log.Warn("artifact with a date in the future; it stays",
				"name", item.Name(), "dir", dir, "date", match[1])
			continue
		}
		path, err := safeFilePath(dir, item.Name())
		if err != nil {
			log.Warn("path would end up outside the directory; skipped", "name", item.Name(), "dir", dir, "error", err)
			continue
		}
		own = append(own, cleanupItem{path: path, date: date})
	}

	// Newest first, then skip the first `minimum`: those always stay, no matter
	// how old (guardrail 6).
	sort.Slice(own, func(i, j int) bool {
		if own[i].date.Equal(own[j].date) {
			return own[i].path > own[j].path
		}
		return own[i].date.After(own[j].date)
	})
	if len(own) <= minimum {
		return nil, nil
	}
	candidates := own[minimum:]

	limit := now.AddDate(0, 0, -keepDays)
	var gone []cleanupItem
	for _, k := range candidates {
		if k.date.Before(limit) {
			gone = append(gone, k)
		}
	}
	// candidates are newest first; reverse so that the oldest goes first. That
	// is the order you want when the budget runs out halfway.
	for i, j := 0, len(gone)-1; i < j; i, j = i+1, j-1 {
		gone[i], gone[j] = gone[j], gone[i]
	}
	return gone, nil
}

// removeDayDir cleans up one day directory with thumbnails: first the files we
// wrote ourselves, then the directory. One level, descended by hand.
//
// Guardrail 4: if the directory holds even one thing we do not recognize, the
// whole directory stays and a WARN follows. A human put something there then,
// and throwing that away is exactly what this program must never do.
func removeDayDir(log *slog.Logger, dir string, dryRun bool) (bool, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	var toDelete []string
	for _, item := range items {
		name := item.Name()
		recognized := dayDirReport.MatchString(name) || thumbnailName.MatchString(name) || tempName.MatchString(name)
		if !recognized || !item.Type().IsRegular() {
			log.Warn("day directory holds something photowatch did not write itself; the whole directory stays",
				"dir", dir, "name", cleanText(name, maxExampleLength),
				"consequence", "this directory is never cleaned up as long as that file is in it")
			return false, nil
		}
		path, err := safeFilePath(dir, name)
		if err != nil {
			return false, err
		}
		toDelete = append(toDelete, path)
	}
	if dryRun {
		log.Info("dry run: would delete day directory with thumbnails", "dir", dir, "files", len(toDelete))
		return true, nil
	}
	for _, path := range toDelete {
		if err := os.Remove(path); err != nil {
			return false, fmt.Errorf("thumbnail %s cannot be deleted: %w", path, err)
		}
	}
	// os.Remove and not os.RemoveAll: this fails when something has been added
	// in the meantime, and that is exactly the behaviour we want.
	if err := os.Remove(dir); err != nil {
		return false, fmt.Errorf("day directory %s cannot be deleted: %w", dir, err)
	}
	log.Debug("day directory with thumbnails deleted", "dir", dir, "files", len(toDelete))
	return true, nil
}

// emptyDayDir removes the result of an earlier round from the day directory —
// the thumbnails *and* the copy of the report — so that number 003 can never
// point at two different photos. Only names that match our own patterns
// exactly; everything else stays (guardrail 3).
//
// On a real run this is in practice a no-op: ChooseArtifactSlot moves to an
// unused stamp, so the day directory is new. It does matter for a dry run,
// because that always writes into the same directory <THUMBNAIL_DIR>/dry-run
// and replaces its own previous result there. That is also the only thing a dry
// run ever deletes.
//
// Why report.txt goes along, while WriteReport will overwrite it in a moment:
// that only happens when a thumbnail succeeded. If not a single one succeeds in
// a dry run, MakeThumbnails removes the empty day directory — but then the
// report of the *previous* dry run was still lying there, os.Remove failed on a
// non-empty directory (only visible at DEBUG) and a report without images
// stayed behind, without any hint that it was old. That directory is exactly
// where somebody is checking whether the scaling works.
func emptyDayDir(log *slog.Logger, dir string) (int, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, item := range items {
		recognized := thumbnailName.MatchString(item.Name()) || dayDirReport.MatchString(item.Name())
		if !recognized || !item.Type().IsRegular() {
			continue
		}
		path, err := safeFilePath(dir, item.Name())
		if err != nil {
			return n, err
		}
		if err := os.Remove(path); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// removeEmptyDayDir removes a day directory MakeThumbnails just created itself
// and that stayed empty because not a single file became a thumbnail. Without
// this, an empty directory sits on the share for sixteen days with the
// notification pointing at it.
//
// os.Remove and not os.RemoveAll, for the same reason as everywhere in this
// file: when there *is* something in it (a report.txt of an earlier round, or
// something from a human), this fails and the directory stays. That is the
// desired behaviour, so a failure here is not an incident but a DEBUG line.
func removeEmptyDayDir(log *slog.Logger, dir string) {
	if err := os.Remove(dir); err != nil {
		log.Debug("empty day directory stays", "dir", dir, "reason", err)
		return
	}
	log.Debug("empty day directory cleaned up", "dir", dir)
}

// From this age on, a temporary file is a leftover of an aborted write and not
// a file of a run that is busy now. A day is generous: a run takes seconds to
// minutes and there is at most one per day.
const maxTempAge = 24 * time.Hour

// tempName matches exactly the names os.CreateTemp makes with the pattern
// ".photowatch-*": the prefix followed by digits. Deliberately that narrow: a
// file somebody named ".photowatch-notes" stays.
var tempName = regexp.MustCompile(`^\.photowatch-[0-9]+$`)

// removeStaleTemps throws away temporary files of an aborted write. The defer
// in writeAtomic covers every error path, but not a SIGKILL, an OOM or a power
// cut between CreateTemp and Rename; nothing else cleans such leftovers up
// later, and the retention only covers snapshots. Without this they pile up
// over the years in the report directory.
//
// The same guardrails as the rest of this file: only in the target directory
// itself (no recursion), only names that match our own pattern exactly, only
// regular files (do not follow a symlink — DirEntry.Info() is lstat), and only
// when they are older than a day, so that the temporary file of a concurrent
// run is never touched.
//
// An error may never hold up writing the report: hence no return value, but a
// log line. That is also why the caller does this before CreateTemp and not
// after.
func removeStaleTemps(log *slog.Logger, dir string, now time.Time) {
	items, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("directory cannot be walked to clean up old temporary files; writing continues",
			"dir", dir, "error", err)
		return
	}
	for _, item := range items {
		if !tempName.MatchString(item.Name()) {
			continue
		}
		info, err := item.Info()
		if err != nil {
			// Can simply happen: the file disappeared between reading the
			// directory and now. No reason for alarm, but worth mentioning.
			log.Debug("temporary file cannot be stat'ed; skipped", "name", item.Name(), "dir", dir, "error", err)
			continue
		}
		if !info.Mode().IsRegular() || now.Sub(info.ModTime()) < maxTempAge {
			continue
		}
		full := filepath.Join(dir, item.Name())
		if err := os.Remove(full); err != nil {
			log.Warn("old temporary leftover cannot be deleted; writing continues",
				"path", full, "error", err)
			continue
		}
		log.Info("leftover of an aborted write cleaned up",
			"path", full, "age_hours", int(now.Sub(info.ModTime()).Hours()))
	}
}

// DirSizeMB returns the size of a directory in whole megabytes. Two levels deep
// is enough for both directories we measure (the report directory is flat, the
// thumbnail directory has one layer of day directories) and it saves a full
// tree walk if the path ever points somewhere else.
func DirSizeMB(dir string) (int, error) {
	size, err := dirSizeBytes(dir, 2)
	return int(size / (1024 * 1024)), err
}

func dirSizeBytes(dir string, depth int) (int64, error) {
	if depth <= 0 {
		return 0, nil
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range items {
		if item.Type()&fs.ModeSymlink != 0 {
			// A symlink does not count: it may point at something outside this
			// directory, and then the number would mean nothing.
			continue
		}
		if item.IsDir() {
			sub, err := dirSizeBytes(filepath.Join(dir, item.Name()), depth-1)
			if err != nil {
				continue // a subdirectory that disappears while measuring is not an error
			}
			total += sub
			continue
		}
		info, err := item.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// isDiskFull recognizes the two problems a full pool or a reached ZFS quota
// produce. They deserve a message of their own, because the usual reaction
// ("the file cannot be written") sends the reader in the wrong direction:
// nothing is broken, the directory is full.
//
// EDQUOT is what ZFS gives on a dataset quota, ENOSPC on a full pool.
// errors.Is walks through the *PathError from os down to the syscall.Errno.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
