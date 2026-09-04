// Package watch is photowatch itself: the daily snapshot, the diff, the
// notification and the paperwork that follows it.
package watch

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Exit codes. 2 explicitly means "misconfigured": retrying does not help
// against that, and it saves searching in journalctl.
const (
	exitOK          = 0
	exitRunError    = 1
	exitConfigError = 2
)

// The text arriving with -report-failure comes from systemd (the name of the
// failed unit) and therefore from outside this program. It is truncated and
// stripped of control characters before it lands in the JSON and in the log.
const maxFailureTextLength = 200

// A systemd invocation id is 32 hex characters. It comes from the environment
// and lands in a log line, so like every other input from outside it is
// truncated and stripped of control characters.
const maxInvocationLength = 64

// Run is the whole program; main() only passes on its exit code.
func Run() int {
	dryRun := flag.Bool("dry-run", false, "compute everything and write the report, the restore script and the thumbnails into the dry-run/ subdirectory; no snapshot, nothing created or deleted outside that directory, nothing sent")
	status := flag.Bool("status", false, "show the last written status.json and stop")
	debug := flag.Bool("debug", false, "verbose logging (level DEBUG)")
	reportFailure := flag.String("report-failure", "", "only send a failure notification with this text to Home Assistant (used by the OnFailure unit)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	// To stderr, because journald picks that up: journalctl -u photowatch.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if rest := flag.Args(); len(rest) > 0 {
		log.Error("unknown arguments given", "arguments", rest)
		flag.Usage()
		return exitConfigError
	}

	cfg := LoadConfig()

	// -status comes before validation: the state file must be readable even when
	// the rest of the configuration is not right yet. That is exactly the moment
	// you look at it.
	if *status {
		return showStatus(cfg, log)
	}

	// A SIGTERM from systemd (on `systemctl stop` or a timeout) aborts the
	// running zfs and HTTP calls cleanly instead of killing the process hard in
	// the middle of a diff.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// -report-failure comes before the full validation, and that is the core of
	// this path: the OnFailure unit is started precisely when something is
	// wrong, and a configuration error is the most likely case. If we validated
	// everything here first, the failure reporter would fall over on exactly the
	// error it had to report. It only validates what it needs: the webhook.
	if *reportFailure != "" {
		return reportFailureOnly(ctx, cfg, log, *reportFailure)
	}

	if err := cfg.Validate(); err != nil {
		log.Error("configuration is not valid, nothing is done",
			"error", err, "env_file", "/etc/photowatch/photowatch.env")
		return exitConfigError
	}

	notifier := NewNotifier(cfg, log)

	start := time.Now()
	p, plan, err := run(ctx, cfg, log, *dryRun)
	// The duration of the run itself, so up to and including building the
	// payload. What the aftercare costs afterwards is in its own log line: this
	// number should say how long it took before the alert could go out.
	p.DurationS = rounded(time.Since(start).Seconds())

	if err != nil {
		log.Error("photowatch ran into a failure", "error", err, "dataset", cfg.Dataset)
		p.Status = "error"
		p.Alert = true
		p.Message = firstLine(err.Error())
	}

	if *dryRun {
		raw, jsonErr := jsonShort(p)
		if jsonErr != nil {
			log.Error("payload cannot be shown", "error", jsonErr)
			return exitRunError
		}
		log.Info("dry run: this is what would go to Home Assistant", "payload", raw)
		// The aftercare runs during a dry run too: it writes the report and the
		// thumbnails, and that is exactly the intent — it lets you check whether
		// the scaling works and whether the snapshot directory is readable.
		// Writing happens only in the dry-run directory (see ChooseArtifactSlot),
		// and a dry run deletes nothing: CleanArtifacts only logs what it would
		// throw away, because DryRun is set in the job.
		runAftercare(ctx, log, plan)
		if err != nil {
			return exitRunError
		}
		return exitOK
	}

	notifyErr := notifier.Send(ctx, p)
	if notifyErr != nil {
		log.Error("reporting to Home Assistant failed", "error", notifyErr,
			"consequence", "HA does not update the sensors; the \"photowatch is silent\" automation raises the alarm in 30 hours")
	}

	// The state file is written even when the notification failed: whoever looks
	// on the host must be able to see that the run itself did happen. Reported
	// records that HA accepted the message; Invocation records *which* start of
	// the unit that was. The OnFailure unit reads those two in a moment so as
	// not to report the same failure twice.
	sf := stateAfterRun(log, cfg, p, notifyErr == nil, plan)
	writeStateOrLog(log, cfg.StateDir, sf)

	// Only here the risky work. Everything above is finished and recorded: the
	// alert is on the phone and status.json is on disk. If this were above it,
	// an OOM kill or a panic while decoding an image file would make the message
	// "43 files are gone" disappear — while the new reference snapshot has
	// already been made and tomorrow's diff counts a tidy zero. That day would
	// then no longer be reportable and nobody would know there is any hurry.
	if plan != nil {
		result := runAftercare(ctx, log, plan)
		sf.Aftercare = &result
		sf.Written = time.Now().Format(time.RFC3339)
		writeStateOrLog(log, cfg.StateDir, sf)
	}

	if err != nil || notifyErr != nil {
		return exitRunError
	}
	return exitOK
}

// stateAfterRun assembles the state file that is written right after the
// notification. It exists as its own function for one reason, and that reason
// is below: carrying over the aftercare of the previous run is a case you only
// notice when it goes wrong.
func stateAfterRun(log *slog.Logger, cfg *Config, p Payload, reported bool, plan *AftercarePlan) StateFile {
	sf := StateFile{
		Written:    time.Now().Format(time.RFC3339),
		Reported:   reported,
		Invocation: cleanText(os.Getenv("INVOCATION_ID"), maxInvocationLength),
		Payload:    p,
	}
	if plan != nil {
		// The ordinary path: the aftercare of this run will be filled in here in
		// a moment, and the side issues of the *previous* run are already in p.
		return sf
	}
	// run() stranded so early (zfs binary, dataset, snapshot list, diff or the
	// snapshot itself) that it could not put the side issues of the previous
	// aftercare into today's payload; that only happens after the snapshot. If
	// we wrote a fresh state without aftercare here, that side issue would have
	// disappeared from every chain and only lived in the journal: day 1 the
	// cleanup fails, day 2 `zfs list` hiccups, and from day 3 on nobody knows
	// about day 1 any more. Only carry it over in this case — if today's payload
	// *was* built, the side issue is already in it and it would otherwise report
	// itself again every day.
	earlier, found, readErr := ReadState(cfg.StateDir)
	switch {
	case readErr != nil:
		log.Warn("state file of the previous run not readable; what that run ran into after its notification is lost",
			"dir", cfg.StateDir, "error", readErr)
	case found && earlier.Aftercare != nil:
		sf.Aftercare = earlier.Aftercare
		log.Debug("aftercare of the previous run carried over because this run stranded early",
			"side_issues", earlier.Aftercare.SideIssues, "artifacts_mb", earlier.Aftercare.ArtifactsMB)
	}
	return sf
}

func writeStateOrLog(log *slog.Logger, dir string, sf StateFile) {
	if path, err := WriteState(log, dir, sf); err != nil {
		log.Error("state file cannot be written", "dir", dir, "error", err)
	} else {
		log.Debug("state written", "path", path)
	}
}

// AftercarePlan is the work that deliberately happens only after the
// notification: making the thumbnails, writing the report and cleaning up the
// artifacts. run() prepares it and Run() executes it as soon as the message
// is out; see the explanation at runAftercare.
type AftercarePlan struct {
	Now     time.Time
	DryRun  bool
	Cleanup CleanupJob

	// Thumbnails: only set when there is a day directory and there is something
	// to make.
	Thumbnails bool
	Source     SnapshotSource
	DayDir     string
	Candidates []DeletedFile
	Options    ThumbnailOptions

	// Plan is the outcome of PlanThumbnails of this run, also when there was
	// nothing to scale. It sits here separately because the report needs it
	// exactly then: if only videos, raw files or XMP sidecars disappear on a
	// day, Thumbnails stays false, MakeThumbnails does not run and the report
	// would stay silent about the reason — while those files should appear in
	// the report with the reason next to them. The same holds for a thumbnail
	// directory that is over THUMBNAIL_MAX_MB: then the reason is in
	// Plan.Message and nowhere else.
	//
	// Nil means: no plan was made this run (no day directory, no mountpoint, or
	// nothing disappeared).
	Plan *ThumbnailPlan

	// Report is nil when no report should be produced this run. ReportDir sits
	// here separately because WriteReport takes the directory as an argument.
	Report    *ReportData
	ReportDir string
}

// run does the actual work and always returns a payload — also on an error, so
// that the caller can report what *was* counted. The second result is the work
// that may only be done after reporting; it is nil when the run stranded so
// early that there was nothing to prepare.
func run(ctx context.Context, cfg *Config, log *slog.Logger, dryRun bool) (Payload, *AftercarePlan, error) {
	now := time.Now()
	newName := SnapshotNameFor(cfg.Dataset, cfg.SnapshotPrefix, now)

	p := newPayload(now)
	p.Status = "ok"
	p.Dataset = cfg.Dataset
	p.NewSnapshot = newName
	p.Threshold = cfg.Threshold

	z := Zfs{Path: cfg.ZfsPath}
	if err := checkZfsBinary(cfg.ZfsPath); err != nil {
		return p, nil, err
	}
	if err := z.DatasetExists(ctx, cfg.Dataset); err != nil {
		return p, nil, fmt.Errorf("dataset %q cannot be found; check DATASET in /etc/photowatch/photowatch.env with `zfs list`: %w", cfg.Dataset, err)
	}

	// problems are failures that do not stop the run — today's snapshot must
	// happen no matter what — but that *are* reported as an error at the end.
	// Without this list such a failure would end as "all is well" and nobody
	// would notice a thing.
	var problems []string

	// The mountpoint is needed for two things: the rsync example in the report,
	// and the validation of PATH_PREFIX below. Query it once.
	mountpoint, mountpointErr := z.Mountpoint(ctx, cfg.Dataset)
	if mountpointErr != nil {
		log.Warn("mountpoint of the dataset cannot be determined", "dataset", cfg.Dataset, "error", mountpointErr,
			"consequence", "the report gets a generic restore example and PATH_PREFIX is not validated this run")
	} else if err := checkPathPrefix(mountpoint, cfg.PathPrefix); err != nil {
		// Keep it short: this goes to the phone as `message`.
		problems = append(problems, "PATH_PREFIX does not match this dataset; nothing is counted")
		log.Error("PATH_PREFIX does not match the dataset; no path from `zfs diff` can fall inside it",
			"error", err, "path_prefix", cfg.PathPrefix, "dataset", cfg.Dataset, "mountpoint", mountpoint,
			"consequence", "the snapshot is still made, but the count of this run means nothing",
			"check", "PATH_PREFIX in /etc/photowatch/photowatch.env against the paths `zfs diff -H -F <previous> <dataset> | head` prints")
	}

	// The thumbnail directory may never lie inside the watched dataset. That is
	// not a matter of taste: `zfs diff` would see the thumbnails as added files
	// tomorrow, and Immich would import them as photos during its next scan of
	// the external library. As with PATH_PREFIX, that check can only happen
	// here, because the mountpoint comes from `zfs get`.
	thumbnailDir := cfg.ThumbnailDir
	if thumbnailDir != "" && mountpointErr == nil {
		if err := checkThumbnailDir(mountpoint, thumbnailDir); err != nil {
			// Keep it short: this goes to the phone as `message`.
			problems = append(problems, "THUMBNAIL_DIR lies inside the watched archive; no thumbnails are made")
			log.Error("THUMBNAIL_DIR lies inside the watched dataset; not a single thumbnail is written",
				"error", err, "thumbnail_dir", thumbnailDir, "mountpoint", mountpoint,
				"consequence", "Immich would import the thumbnails as photos and zfs diff would count them as added tomorrow",
				"check", "THUMBNAIL_DIR in /etc/photowatch/photowatch.env; pick a dataset of its own next to the photo dataset")
			thumbnailDir = ""
		}
	}

	// The state file of the previous run, read once. It serves two purposes:
	// below, the question "has a run ever finished" (that decides whether a
	// missing reference snapshot is the first time or a failure), and further on
	// the side issues the *previous* run only ran into after its notification
	// and that could therefore go nowhere else.
	earlier, ranBefore, stateErr := ReadState(cfg.StateDir)
	if stateErr != nil {
		// Not readable: then we do not know, and then we assume the worst. One
		// notification too many is better than silence here.
		log.Warn("state file not readable; assuming a run has happened before",
			"dir", cfg.StateDir, "error", stateErr)
		ranBefore = true
	}

	snaps, unparsedListLines, err := z.SnapshotList(ctx, cfg.Dataset)
	if err != nil {
		return p, nil, err
	}
	if unparsedListLines > 0 {
		// Do not throw away silently: every line we do not understand can be the
		// reference snapshot we should have diffed against.
		log.Warn("lines from `zfs list` could not be parsed; a snapshot may be missing from the count",
			"count", unparsedListLines, "dataset", cfg.Dataset)
	}
	log.Debug("snapshots found", "dataset", cfg.Dataset, "count", len(snaps))

	previous, hasPrevious := LatestWithPrefix(snaps, cfg.SnapshotPrefix)
	diff := &DiffResult{}

	if !hasPrevious {
		// No reference. That can mean two very different things, and the
		// difference is in status.json: if no run ever finished, this is simply
		// the first time. If that file *does* exist, the reference snapshot has
		// disappeared and nothing was compared today — and that may never be lost
		// as "first run, 0 deleted".
		if ranBefore {
			// Keep it short: this text goes to Home Assistant as `message` and
			// ends up on a phone screen, where it is cut off at 200 characters.
			// The explanation with dataset, prefix and time is in the ERROR line
			// below, in the journal.
			problems = append(problems, fmt.Sprintf(
				"reference snapshot gone (last run %s): nothing was compared today, only a new reference was set",
				earlierOrUnknown(earlier.Written)))
			log.Error("reference snapshot missing while a run has happened before",
				"dataset", cfg.Dataset, "prefix", cfg.SnapshotPrefix, "last_run", earlierOrUnknown(earlier.Written),
				"consequence", "this run compares nothing and only sets a new reference; tomorrow it measures over 24 hours again")
		} else {
			// First run: there is nothing to compare against. Only set the
			// reference and say so honestly, so that an empty count is not read as
			// "nothing is gone".
			p.Message = "first run: reference snapshot set, there was nothing to compare with yet"
			log.Info("no earlier snapshot with this prefix and no status.json yet; this is the first run",
				"dataset", cfg.Dataset, "prefix", cfg.SnapshotPrefix, "snapshot", newName)
		}
	} else {
		p.PreviousSnapshot = previous.Name
		p.Since = previous.Created.Format(time.RFC3339)
		log.Debug("diffing against the previous snapshot", "previous", previous.Name, "since", p.Since)

		diff, err = z.Diff(ctx, previous.Name, cfg.Dataset, cfg.PathPrefix)
		if err != nil {
			return p, nil, err
		}
		p.Deleted = len(diff.Deleted)
		p.DeletedDirs = diff.DeletedDirs
		p.DeletedOther = diff.DeletedOther
		p.Renamed = diff.Renamed
		p.Added = diff.Added
		p.Alert = p.Deleted >= cfg.Threshold
		p.Examples = Examples(diff.Deleted)
		p.Unparsed = diff.Unparsed
		p.Skipped = diff.Skipped

		if diff.Unparsed > 0 {
			log.Warn("lines from `zfs diff` could not be parsed",
				"unparsed", diff.Unparsed, "recognized", recognizedLines(diff), "previous", previous.Name)
		}
		if diffUnusable(diff) {
			// Keep it short: this goes to the phone as `message`. The numbers and
			// the hint about what to check are in the ERROR line.
			problems = append(problems, "not a single line from `zfs diff` was counted; the count of this run means nothing")
			log.Error("not a single line from `zfs diff` was counted",
				"unparsed", diff.Unparsed, "outside_path_prefix", diff.Skipped,
				"path_prefix", cfg.PathPrefix, "dataset", cfg.Dataset, "previous", previous.Name,
				"check", "PATH_PREFIX against the paths `zfs diff -H -F <previous> <dataset> | head` prints, and the mountpoint of the dataset")
		}
	}

	// The snapshot before the report. The other way around, a failed snapshot
	// would leave behind a report pointing at something that does not exist —
	// and it is exactly that report that holds the rsync line to run.
	if err := makeSnapshot(ctx, z, log, newName, now, &snaps, dryRun); err != nil {
		return p, nil, err
	}

	// From here on the side issues: restore script, thumbnails, report and the
	// cleanup of artifacts. Ground rule: none of this may hold up the
	// notification. The alert is the product; a thumbnail is comfort. An error
	// therefore becomes a WARN in the journal *and* a line in the payload, but
	// never a `status: error` that drowns out the real message.
	//
	// What happens here is writing text files; the risky work (decoding image
	// files from outside, deleting files) is only *prepared* and executed after
	// the notification. See runAftercare.
	var restore RestoreResult
	var recoverUntil time.Time
	var sideIssues []string
	plan := &AftercarePlan{Now: now, DryRun: dryRun}

	// What the *previous* run only ran into after its notification had nowhere
	// to go at the time. It travels along today, with "on the previous run" in
	// front of it so that nobody thinks it happened this morning. A failure that
	// persists (the cleanup fails structurally) still reaches the phone every
	// day this way.
	//
	// It is kept separate and only appended behind today's side issues at the
	// bottom. The combination is truncated at maxFailureTextLength, and if this
	// item were first, exactly the one sentence you need at that moment would
	// fall off: "second run today: … these are called 2026-09-01-2", the
	// sentence that explains why the message points at names with a sequence
	// number. What happened today therefore goes before what happened
	// yesterday; the journal and status.json keep the full text anyway.
	var previousItem string
	if earlier.Aftercare != nil && earlier.Aftercare.SideIssues != "" {
		previousItem = "on the previous run: " + earlier.Aftercare.SideIssues
	}

	// Where the artifacts of this run go and what they are called. Chosen once
	// and used everywhere afterwards, so that the path in the notification and
	// the path the file ends up at cannot drift apart — and so that a second run
	// on the same day does not overwrite the set of the first. See ArtifactSlot
	// in report.go.
	//
	// Only when there really is something to write. Choosing does an Lstat in
	// THUMBNAIL_DIR, and on an ordinary day (nothing gone) nothing should happen
	// to that directory before the notification: if it ever points at a network
	// mount, a hung mount holds up the alert. The condition is the same as that
	// of the report block further down — the wider of the two blocks that use
	// the slot.
	somethingToWrite := hasPrevious && (p.Deleted > 0 || diff.DeletedDirs > 0)
	var slot ArtifactSlot
	var slotErr error
	if somethingToWrite {
		slot, slotErr = ChooseArtifactSlot(cfg.ReportDir, thumbnailDir, now, dryRun)
	}
	switch {
	case !somethingToWrite:
		// Nothing disappeared; then there is no report, no restore script and no
		// day directory, and nothing to choose or report either.
	case slotErr != nil:
		// Mention the thumbnails too: a failed slot also blocks the day
		// directory, so there will not be a single image today. Without that,
		// the reader would look for a day directory at the usual path (known
		// from yesterday's message) that never came into being. What the ERROR
		// line below says in the journal belongs on the phone as well.
		sideIssues = append(sideIssues, "no slot for report, restore script and thumbnails")
		log.Error("no slot for the artifacts of this run; there will be no report, no restore script and no thumbnail",
			"error", slotErr, "report_dir", cfg.ReportDir, "thumbnail_dir", thumbnailDir,
			"consequence", "the notification and the snapshot happen as usual; only the side artifacts are missing",
			"check", "whether "+cfg.ReportDir+" is readable and how many sets of today are already there")
	case dryRun:
		// The dry-run directory is created here and not by the writers
		// themselves: os.MkdirAll gives a new directory the group of the process
		// (root), and then the outcome of your own dry run is unreadable.
		if err := makeDirWithGroup(log, slot.Dir); err != nil {
			sideIssues = append(sideIssues, "dry-run directory cannot be created")
			log.Error("dry-run directory cannot be created; this dry run writes nothing",
				"dir", slot.Dir, "error", err)
			slotErr = err
		} else {
			log.Info("dry run: everything goes to the dry-run directory", "dir", slot.Dir, "day_dir", slot.DayDir)
		}
	case !slot.First(now):
		// Make it visible, not only in the journal: the notification now points
		// at names with a sequence number behind them, and nobody should have to
		// guess that.
		sideIssues = append(sideIssues, "second run today: the artifacts of the earlier run stay, these are called "+slot.Stamp)
		log.Warn("there are already artifacts of today; this run writes under a name of its own",
			"stamp", slot.Stamp, "report_dir", cfg.ReportDir,
			"consequence", "the earlier set is left alone; the notification of that run therefore stays correct")
	}
	// One flag for the three blocks below: was a slot chosen *and* did that go
	// well? Without this flag, "slotErr == nil" would also be true on a day when
	// there was nothing to write, and then a zero-value ArtifactSlot would be
	// passed on.
	artifactsOK := somethingToWrite && slotErr == nil

	if hasPrevious && p.Deleted > 0 {
		p.MediaFiles = diff.MediaFiles()
		p.SidecarFiles = diff.SidecarFiles()
		p.Folders = Folders(diff.Deleted, mountpoint, maxFolders)
		// Yesterday's snapshot stays until KEEP_DAYS after its creation; that is
		// the date on which restoring is no longer possible.
		recoverUntil = previous.Created.AddDate(0, 0, cfg.KeepDays)
		p.RecoverUntil = recoverUntil.Format("2006-01-02")

		source := SnapshotSource{Mountpoint: mountpoint, Dir: SnapshotDirFor(mountpoint, previous.Short)}

		// The restore script and the list. This touches the action itself: it is
		// what gets executed later. That is why it comes before the notification
		// and before everything else — it has to exist by the moment the message
		// is on the phone.
		if mountpoint != "" && artifactsOK {
			restore, err = WriteRestore(log, RestoreInput{
				Dir:           slot.Dir,
				ScriptName:    slot.ScriptName(),
				ListName:      slot.ListName(),
				DryRun:        dryRun,
				Now:           now,
				Dataset:       cfg.Dataset,
				Mountpoint:    mountpoint,
				SnapshotShort: previous.Short,
				PreviousTime:  previous.Created,
				RecoverUntil:  recoverUntil,
				SnapshotDir:   source.Dir,
				ZfsPath:       cfg.ZfsPath,
				Deleted:       diff.Deleted,
				MediaFiles:    diff.MediaFiles(),
			})
			if err != nil {
				sideIssues = append(sideIssues, "restore script could not be written — see the report")
				log.Warn("restore script cannot be written; the report gives the manual rsync example",
					"dir", cfg.ReportDir, "error", err)
			} else if restore.Message != "" {
				sideIssues = append(sideIssues, "restore script could not be written — see the report")
			}
			p.RestoreScript = restore.ScriptPath
		} else if mountpoint == "" {
			sideIssues = append(sideIssues, "mountpoint unknown: no restore script and no thumbnails")
		}

		// The thumbnails are only *prepared* here. The path to the day directory
		// and the count are fixed by that and can travel in the notification;
		// opening and decoding the files only happens afterwards.
		if slot.DayDir != "" && mountpoint != "" && artifactsOK {
			opts := ThumbnailOptions{
				Max:        cfg.ThumbnailMax,
				Px:         cfg.ThumbnailPx,
				MaxTotalMB: cfg.ThumbnailMaxMB,
			}
			thumbPlan := PlanThumbnails(slot.DayDir, diff.Deleted, opts)
			// Pass it on even when nothing was chosen: the plan carries the
			// reasons why certain files get no thumbnail, and on such a day those
			// are the only thing there is to say about the thumbnails.
			// runAftercare fills the ThumbnailResult from it when MakeThumbnails
			// does not run.
			plan.Plan = &thumbPlan
			if thumbPlan.Message != "" {
				// For instance: the thumbnail directory is at its limit. That is
				// already known now and therefore belongs in today's message, not
				// only in tomorrow's.
				sideIssues = append(sideIssues, thumbPlan.Message)
			}
			if len(thumbPlan.Chosen) > 0 {
				plan.Thumbnails = true
				plan.Source = source
				plan.DayDir = slot.DayDir
				plan.Candidates = diff.Deleted
				plan.Options = opts
				// The path may appear in the notification before the images are
				// there: it is fixed, and they arrive within seconds. The count is
				// an intention — how many it became is in the journal and in
				// status.json, because we only know that after sending.
				p.Thumbnails = slot.DayDir
				p.ThumbnailsPlanned = len(thumbPlan.Chosen)
			}
		}
	}

	// The report is prepared and only written after the notification — then the
	// thumbnail numbers are in it and the copy can sit next to the images on the
	// share. The path is already fixed now and travels in the payload.
	if artifactsOK {
		// Also below the threshold: the report exists to be able to check
		// afterwards what happened that day. artifactsOK covers the condition
		// that used to be here (there is a previous snapshot *and* a file or a
		// directory disappeared); see somethingToWrite above.
		path, err := slot.ReportPath()
		if err != nil {
			sideIssues = append(sideIssues, "report could not be written")
			log.Error("path for the report cannot be determined", "dir", slot.Dir, "error", err)
		} else {
			p.Report = path
			plan.ReportDir = slot.Dir
			plan.Report = &ReportData{
				Name:             slot.ReportName(),
				Dataset:          cfg.Dataset,
				PathPrefix:       cfg.PathPrefix,
				PreviousSnapshot: previous.Name,
				PreviousShort:    previous.Short,
				PreviousTime:     previous.Created,
				NewSnapshot:      newName,
				Now:              now,
				Mountpoint:       mountpoint, // empty when it could not be queried above
				Threshold:        cfg.Threshold,
				Alert:            p.Alert,
				DryRun:           dryRun,
				Diff:             diff,
				RecoverUntil:     recoverUntil,
				Restore:          restore,
			}
		}
	}

	// The cleanup of files is prepared. It runs on *every* run, also on a day
	// without deletions — otherwise the pile on the hypervisor simply keeps
	// growing — but only after the notification: it is the second place where
	// this program does irreversible work.
	plan.Cleanup = CleanupJob{
		ReportDir:        cfg.ReportDir,
		ThumbnailDir:     thumbnailDir,
		StateDir:         cfg.StateDir,
		Mountpoint:       mountpoint,
		KeepDaysReport:   cfg.KeepDaysReport,
		ArtifactKeepDays: cfg.KeepDays + ArtifactDayMargin,
		DryRun:           dryRun,
	}

	// artifacts_mb should level off after the first two weeks; if it keeps
	// growing, the cleanup is not working. An automation in HA complains above
	// 800 MB. The number comes from the aftercare of the *previous* run and is
	// not measured here: measuring is a walk over THUMBNAIL_DIR, and that should
	// not happen before the notification. If that directory ever points at a
	// network mount, a hung mount would hold up the alert until
	// TimeoutStartSec. For a trend over weeks a day of age is nothing.
	if earlier.Aftercare != nil {
		p.ArtifactsMB = earlier.Aftercare.ArtifactsMB
	}
	if previousItem != "" {
		sideIssues = append(sideIssues, previousItem)
	}
	if len(sideIssues) > 0 {
		p.SideIssues = cleanText(strings.Join(sideIssues, "; "), maxFailureTextLength)
	}

	// Pruning snapshots stays before the notification: it is not reading files
	// from outside but the same zfs work as the rest of the run, and it belongs
	// to the result that is reported.
	pruned, err := pruneSnapshots(ctx, z, cfg, log, snaps, now, dryRun)
	if err != nil {
		return p, plan, err
	}

	log.Info("photowatch done",
		"dataset", cfg.Dataset,
		"previous_snapshot", p.PreviousSnapshot,
		"new_snapshot", newName,
		"deleted", p.Deleted,
		"deleted_dirs", diff.DeletedDirs,
		"deleted_other", diff.DeletedOther,
		"renamed", diff.Renamed,
		"added", diff.Added,
		// Outside the path prefix and unparsed belong in the summary and not
		// only in the report: when there is no report (because nothing seemed
		// deleted), this line is the only place where it shows that there *were*
		// lines but none of them counted.
		"outside_path_prefix", diff.Skipped,
		"unparsed", diff.Unparsed,
		"threshold", cfg.Threshold,
		"alert", p.Alert,
		"pruned", pruned,
		// The thumbnails, the report and the file cleanup are not in this line
		// but in "aftercare done": that work only starts once the message has
		// been sent.
		"thumbnails_planned", p.ThumbnailsPlanned,
		// The figure of the previous run; this run only measures it in the
		// aftercare.
		"artifacts_mb_reported", p.ArtifactsMB,
		"dry_run", dryRun,
		"duration_s", rounded(time.Since(now).Seconds()))

	if len(problems) > 0 {
		// The work is done (the snapshot is there), but something *is* wrong.
		// Returning it as an error makes the payload get status "error", makes
		// the phone ring and puts the unit in systemd on failed. The message text
		// in Home Assistant shows the count alongside, so that a configuration
		// error in a side issue never drowns out "43 files are gone".
		return p, plan, errors.New(strings.Join(problems, "; "))
	}
	return p, plan, nil
}

// runAftercare does the work that deliberately happens only after the
// notification: making the thumbnails, writing the report and cleaning up the
// old artifacts.
//
// Why that order. Everything above — the diff, the new snapshot, the restore
// script, the payload — is finished and recorded by the time this function
// starts. The work here is the riskiest of the program: it opens arbitrary
// image files from the archive and decodes them as root, with a MemoryMax of 1
// GB above it, and it deletes files. If that happened before reporting, an OOM
// kill or a panic on the day 43 files disappear would cost the notification,
// the report *and* the restore script — while the new reference snapshot has
// already been made and tomorrow's diff therefore counts a tidy zero. That day
// could then only be recovered by hand from `zfs diff S1 S2`, and nobody knows
// at that moment that this is needed.
//
// What it costs: the outcome of this work is no longer in today's message. It
// goes to the journal and to status.json, and the *next* run puts the side
// issues in its own side_issues field — behind those of its own run, because
// that text is truncated and what happened today weighs more.
func runAftercare(ctx context.Context, log *slog.Logger, plan *AftercarePlan) AftercareResult {
	if plan == nil {
		return AftercareResult{}
	}
	start := time.Now()
	var result AftercareResult
	var sideIssues []string
	var thumbs ThumbnailResult

	// The plan from before the notification is the basis of what the report says
	// about the thumbnails. If MakeThumbnails runs below, it overwrites this
	// with its own outcome (which holds the same plan data, because it uses the
	// same planning function). If it does *not* run — no deleted file was a
	// jpg/png/gif, or the thumbnail directory was over its limit — then this is
	// the only place the reasons come from.
	if plan.Plan != nil {
		thumbs = ThumbnailResult{
			Candidates: plan.Plan.Candidates,
			Reasons:    plan.Plan.Reasons,
			Message:    plan.Plan.Message,
			Numbers:    map[string]int{},
		}
	}

	if plan.Thumbnails {
		var err error
		thumbs, err = MakeThumbnails(ctx, log, plan.Source, plan.DayDir, plan.Candidates, plan.Options)
		if err != nil {
			sideIssues = append(sideIssues, "thumbnails could not be made")
			log.Warn("making thumbnails failed; the notification and the restore script were already there",
				"dir", plan.DayDir, "error", err)
		}
		if thumbs.Message != "" {
			sideIssues = append(sideIssues, thumbs.Message)
		}
		result.Thumbnails = thumbs.Made
	}

	if plan.Report != nil {
		// The thumbnail numbers and the copy on the share can only happen now:
		// the report refers with [001] to the images that are there by now.
		plan.Report.Thumbnail = thumbs
		plan.Report.CopyDir = thumbs.DayDir // empty when no thumbnails were made
		path, err := WriteReport(log, plan.ReportDir, *plan.Report)
		if err != nil {
			// Without a report the notification is still valuable — it has
			// already been sent — but this must be loud in the log *and* in
			// tomorrow's payload, because the report is the long-term record.
			sideIssues = append(sideIssues, "report could not be written")
			log.Error("report cannot be written", "dir", plan.ReportDir, "error", err)
		} else {
			log.Info("report written", "path", path, "lines", len(plan.Report.Diff.Deleted))
		}
	}

	cleaned, err := CleanArtifacts(log, plan.Cleanup, plan.Now)
	if err != nil {
		// A rejected target directory is a configuration error. It may not stop
		// the run, but it may not stay stuck in the journal either: as long as it
		// persists, it is in tomorrow's message.
		sideIssues = append(sideIssues, "cleanup skipped: "+firstLine(err.Error()))
		log.Error("nothing was cleaned up because a target directory did not pass validation",
			"error", err, "report_dir", plan.Cleanup.ReportDir, "thumbnail_dir", plan.Cleanup.ThumbnailDir)
	} else if cleaned.Message != "" {
		sideIssues = append(sideIssues, cleaned.Message)
	}
	result.CleanedUp = cleaned.Total()

	// Only measure here: after the cleanup of this run, and after the
	// notification. The number travels in tomorrow's message.
	mb, measureErr := artifactsMB(log, plan.Cleanup.ReportDir, plan.Cleanup.ThumbnailDir)
	result.ArtifactsMB = mb
	if measureErr != nil {
		sideIssues = append(sideIssues, firstLine(measureErr.Error()))
	}

	if len(sideIssues) > 0 {
		result.SideIssues = cleanText(strings.Join(sideIssues, "; "), maxFailureTextLength)
	}

	log.Info("aftercare done",
		"thumbnails", result.Thumbnails,
		"cleaned_up_files", result.CleanedUp,
		"artifacts_mb", result.ArtifactsMB,
		"side_issues", result.SideIssues,
		"dry_run", plan.DryRun,
		"duration_s", rounded(time.Since(start).Seconds()))
	return result
}

func recognizedLines(d *DiffResult) int {
	return len(d.Deleted) + d.DeletedDirs + d.DeletedOther + d.Renamed + d.Added + d.Modified
}

// diffUnusable reports whether the output of `zfs diff` as a whole produced
// nothing because we did not understand it: there were lines, but not one could
// be parsed. That happens when the column layout of `zfs diff` changes, and
// from the outside it looks like "nothing happened" — all counters 0, a tidy
// "deleted: 0", a heartbeat that keeps running. So: a failure.
//
// This used to include "everything fell outside PATH_PREFIX". That was removed,
// because with DATASET=tank and PATH_PREFIX=/mnt/tank/photos exactly that is a
// perfectly normal day: written outside the photo directory, nothing inside it.
// The watch then raised a false alarm while nothing was wrong. The real case
// that rule had to catch — PATH_PREFIX no longer matches anything — is now
// caught when the run starts by checkPathPrefix, and that is both cheaper and
// sharper: it says at once what is wrong instead of deriving it afterwards from
// a count.
//
// Individual unparsed lines next to understood lines are *not* a reason for
// alarm; those are counted in the report and in the payload.
func diffUnusable(d *DiffResult) bool {
	return recognizedLines(d) == 0 && d.Unparsed > 0
}

// checkPathPrefix checks at startup whether PATH_PREFIX *can* match the paths
// `zfs diff` prints at all. It prints paths as <mountpoint of the
// dataset>/<path inside the dataset>, so a prefix that does not fall under the
// mountpoint or that does not exist as a directory matches nothing — and then
// the watch tidily counts zero every day without ever measuring anything again.
//
// Two checks, because they each catch something different: the mountpoint
// catches a moved or renamed dataset, the existence of the directory catches a
// typo deeper in the path (/mnt/tank/Photos instead of /mnt/tank/photos).
// Deliberately Lstat and not Stat: if PATH_PREFIX is a symlink, `zfs diff`
// still prints the real path and the prefix never matches — which is just as
// much a mistake.
//
// An empty PATH_PREFIX is not an error but the default: then everything counts.
func checkPathPrefix(mountpoint, prefix string) error {
	if prefix == "" {
		return nil
	}
	mp := filepath.Clean(mountpoint)
	p := filepath.Clean(prefix)
	// The prefix *may* equal the mountpoint (that is the same as no prefix, but
	// it is not an error). The mountpoint "/" separately, because then
	// withinPathPrefix would compare on "//" and never match.
	if mp != "/" && !withinPathPrefix(mp, p) {
		return fmt.Errorf("PATH_PREFIX %q does not fall under the mountpoint %q of the dataset", prefix, mountpoint)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("PATH_PREFIX %q cannot be opened: %w", p, err)
	}
	if !fi.Mode().IsDir() {
		return fmt.Errorf("PATH_PREFIX %q is not an ordinary directory (%s)", p, fi.Mode().Type())
	}
	return nil
}

// checkThumbnailDir is the second half of the validation of THUMBNAIL_DIR: the
// half that can only happen once the mountpoint of the dataset is known. The
// first half (absolute path, not a system directory, not equal to REPORT_DIR or
// STATE_DIR) lives in config.go.
//
// If the directory lies inside the mountpoint, `zfs diff` would see the
// thumbnails as added files tomorrow *and* Immich would import them as photos
// during its next scan of the external library. That last one is the expensive
// mistake: copies of exactly the deleted photos would then be in the library.
func checkThumbnailDir(mountpoint, dir string) error {
	if mountpoint == "" || dir == "" {
		return nil
	}
	mp := filepath.Clean(mountpoint)
	d := filepath.Clean(dir)
	if mp == "/" {
		// A dataset on / is not a setup that occurs here, and then every
		// directory would lie "inside the mountpoint". No validation, but an
		// error: better to refuse than to let it through silently.
		return fmt.Errorf("the mountpoint of the dataset is /; do not pick a THUMBNAIL_DIR then (%s)", d)
	}
	if withinPathPrefix(mp, d) {
		return fmt.Errorf("THUMBNAIL_DIR %q lies in or on the mountpoint %q of the watched dataset", d, mp)
	}
	return nil
}

// artifactsMB adds up the report directory and the thumbnail directory, in the
// aftercare and therefore after the cleanup of this run.
//
// A directory that cannot be measured counts as 0 — but that may not happen
// silently. An automation is waiting on this number that complains above 800
// MB; if the thumbnail directory is unreadable, artifacts_mb only reports the
// report directory and that threshold can by definition never be reached again
// while the share fills up without limit. Hence WARN instead of DEBUG, and
// hence the error coming back: it becomes a side issue and is in tomorrow's
// message.
func artifactsMB(log *slog.Logger, reportDir, thumbnailDir string) (int, error) {
	total := 0
	var unmeasurable []string
	for _, dir := range []string{reportDir, thumbnailDir} {
		if dir == "" {
			continue
		}
		mb, err := DirSizeMB(dir)
		if err != nil {
			log.Warn("size of an artifact directory cannot be determined; it counts as 0",
				"dir", dir, "error", err,
				"consequence", "the \"photowatch disk space\" automation no longer sees this directory grow",
				"check", "whether the directory exists and is readable: ls -ld "+dir)
			unmeasurable = append(unmeasurable, dir)
			continue
		}
		total += mb
	}
	if len(unmeasurable) > 0 {
		return total, fmt.Errorf("size of %s cannot be measured; artifacts_mb is too low",
			strings.Join(unmeasurable, " and "))
	}
	return total, nil
}

// earlierOrUnknown turns an empty timestamp from an old state file into
// readable text; it ends up in a message on a phone screen.
func earlierOrUnknown(t string) string {
	if strings.TrimSpace(t) == "" {
		return "unknown"
	}
	return t
}

// makeSnapshot creates today's snapshot and adds it to the list, so that the
// retention counts it as "newest" and does not accidentally prune one day too
// far back.
func makeSnapshot(ctx context.Context, z Zfs, log *slog.Logger, name string, now time.Time, snaps *[]Snapshot, dryRun bool) error {
	if dryRun {
		log.Info("dry run: snapshot not made", "snapshot", name)
		return nil
	}
	err := z.Snapshot(ctx, name)
	switch {
	case errors.Is(err, ErrSnapshotExists):
		// Second run on the same day. Not an error: running again must be
		// harmless.
		log.Info("snapshot already existed, this is the second run today", "snapshot", name)
		return nil
	case err != nil:
		return err
	}
	log.Info("snapshot made", "snapshot", name)
	*snaps = append(*snaps, Snapshot{Name: name, Short: shortSnapName(name), Created: now})
	return nil
}

func pruneSnapshots(ctx context.Context, z Zfs, cfg *Config, log *slog.Logger, snaps []Snapshot, now time.Time, dryRun bool) (int, error) {
	gone, backlog := SnapshotsToDelete(snaps, cfg.Dataset, cfg.SnapshotPrefix, cfg.KeepDays, now)
	if backlog {
		log.Warn("more old snapshots than may go per run; the rest follows in the coming days",
			"max_per_run", MaxDeletePerRun, "keep_days", cfg.KeepDays)
	}
	pruned := 0
	for _, name := range gone {
		if dryRun {
			log.Info("dry run: would delete snapshot", "snapshot", name)
			continue
		}
		if err := z.Destroy(ctx, name); err != nil {
			return pruned, fmt.Errorf("old snapshot %s cannot be deleted: %w", name, err)
		}
		log.Info("old snapshot pruned", "snapshot", name, "older_than_days", cfg.KeepDays)
		pruned++
	}
	return pruned, nil
}

// reportFailureOnly is the path the OnFailure unit uses: no zfs, no snapshot,
// only a message that the task fell over. That way you also hear about it when
// the program stumbles so early that it could no longer report anything itself.
func reportFailureOnly(ctx context.Context, cfg *Config, log *slog.Logger, text string) int {
	text = cleanText(text, maxFailureTextLength)

	// If *this* failed run already reported itself, we leave it at that:
	// otherwise two messages arrive for one failure. The run writes status.json
	// after sending, with reported=true, status "error" and its own
	// $INVOCATION_ID in it. Reported says no more than "Home Assistant accepted
	// the message" — HA also answers 200 to an unknown webhook id — but this
	// side cannot get any further than that.
	monitor := cleanText(os.Getenv("MONITOR_INVOCATION_ID"), maxInvocationLength)
	if earlier, found, err := ReadState(cfg.StateDir); err != nil {
		// Not readable: then we do not know and we would rather report twice than
		// not at all. Log it, because the state file should be readable.
		log.Warn("state file not readable; reporting this failure anyway",
			"dir", cfg.StateDir, "error", err)
	} else if found && sameRun(earlier, monitor) {
		log.Info("the failed run already reported itself to Home Assistant; no second message",
			"message_of_the_run", earlier.Payload.Message, "state_written", earlier.Written,
			"invocation", monitor, "unit", text)
		return exitOK
	}

	// Deliberately not the full Validate(): an error in DATASET, THRESHOLD or
	// REPORT_DIR must specifically not stop the reporting — *that* is the news.
	// Only the webhook is really needed; without it there is no way out.
	if err := cfg.checkWebhook(); err != nil {
		log.Error("the failure notification cannot be sent: WEBHOOK_URL is not valid",
			"error", err, "env_file", "/etc/photowatch/photowatch.env", "to_report", text)
		return exitConfigError
	}
	if err := cfg.Validate(); err != nil {
		// The rest of the configuration is broken; put that in the message, so
		// that the cause is on the phone screen right away.
		text = cleanText(text+" — "+firstLine(err.Error()), maxFailureTextLength)
	}

	// newPayload sets Time, and on this path that is the only field that differs
	// per run: otherwise this message is word for word the same for a failure
	// that repeats every day.
	p := newPayload(time.Now())
	p.Status = "error"
	p.Dataset = cleanText(cfg.Dataset, maxFailureTextLength)
	p.Threshold = cfg.Threshold
	p.Alert = true
	p.Message = text
	log.Error("passing a failure notification to Home Assistant", "message", text)
	notifier := NewNotifier(cfg, log)
	if err := notifier.Send(ctx, p); err != nil {
		log.Error("failure notification could not be sent", "error", err)
		return exitRunError
	}
	return exitOK
}

// sameRun reports whether the state file is about exactly the run that is now
// being reported as failed, *and* whether that run already reported itself.
//
// The comparison runs over systemd's invocation id: every start of a unit gets
// one, and systemd passes that of the failed unit to its OnFailure unit in
// $MONITOR_INVOCATION_ID (systemd 249 and newer). That is exactly what we want
// to know. This used to be "status.json is less than fifteen minutes old", and
// that was just wrong: if the run fell over twice within fifteen minutes and
// the second could no longer report itself, the OnFailure unit stayed silent
// about that second failure — exactly the case it exists for.
//
// If either of the two ids is missing (older systemd, or started by hand), we
// always report. One message too many is annoying, a missed failure is
// expensive.
func sameRun(s StateFile, monitorInvocation string) bool {
	if monitorInvocation == "" || s.Invocation == "" {
		return false
	}
	return s.Invocation == monitorInvocation && s.Reported && s.Payload.Status == "error"
}

func showStatus(cfg *Config, log *slog.Logger) int {
	path, err := safeFilePath(cfg.StateDir, "status.json")
	if err != nil {
		log.Error("state directory is not valid", "dir", cfg.StateDir, "error", err)
		return exitConfigError
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Error("state file not readable; has photowatch run yet?", "path", path, "error", err)
		return exitRunError
	}
	if _, err := os.Stdout.Write(data); err != nil {
		log.Error("state cannot be written to stdout", "error", err)
		return exitRunError
	}
	return exitOK
}

// checkZfsBinary gives an understandable error instead of the bare "no such
// file or directory" exec would otherwise return for every command.
func checkZfsBinary(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the zfs command is not at %s; check with `command -v zfs` and set ZFS_PATH in /etc/photowatch/photowatch.env: %w", path, err)
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable; is ZFS_PATH right?", path)
	}
	return nil
}

// cleanText removes control characters (including newlines) from text from
// outside and truncates it. Without this, one log line can look like several in
// journald, which is a fine way to forge a message.
func cleanText(s string, max int) string {
	return shortText(strings.TrimSpace(stripControlChars(s)), max)
}

// stripControlChars replaces control characters with a space without doing
// anything else to the text. Separate from cleanText because the report shows
// paths that may start or end with a space; those may not be trimmed.
func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return shortText(strings.TrimSpace(line), maxFailureTextLength)
}

func shortSnapName(fullName string) string {
	_, short, _ := strings.Cut(fullName, "@")
	return short
}

func rounded(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
