package watch

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The restore script is executable (750): root writes it, the admin group may
// read and run it, and nobody else can change it. The list file is plain data
// and gets 640.
const (
	modeScript = 0o750
	modeList   = 0o640
)

// Values that end up literally in the generated script must fully match this.
// It is the second of the two safeguards on generating executable text — the
// first is that no file name at all appears in the script; those travel NUL
// separated through --files-from.
//
// No @ and no : in the set, and that is deliberate: the full snapshot name
// (dataset@name) therefore does not enter the script as one value but is
// assembled there from two validated halves. That way the allowlist does not
// have to be wider than strictly necessary.
var scriptValueRegexp = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// RestoreInput is everything needed to write the script and the list.
type RestoreInput struct {
	Dir string // where the script and the list go
	// The two file names come from the ArtifactSlot of the run and are not
	// derived from Now here: the path to the script is in the notification on
	// the phone, and a second run on the same day may not replace that script
	// with a script about entirely different files.
	ScriptName    string
	ListName      string
	DryRun        bool      // shown in the header of the script; no other difference
	Now           time.Time // the date in the header and in the instructions
	Dataset       string
	Mountpoint    string
	SnapshotShort string    // the part after the @
	PreviousTime  time.Time // creation time of that snapshot
	RecoverUntil  time.Time // until when the snapshot stays
	SnapshotDir   string    // <mountpoint>/.zfs/snapshot/<short>
	ZfsPath       string
	Deleted       []DeletedFile
	MediaFiles    int
}

// RestoreResult tells the caller what was written, and why not when it failed.
type RestoreResult struct {
	ScriptPath string
	ListPath   string
	Lines      int
	Message    string         // empty when everything went well
	Reasons    map[string]int // reason -> number of skipped paths
}

// WriteRestore puts down the list file and the restore script.
//
// The core of why this is safe: the file names never pass a shell. They sit NUL
// separated in the list file, which rsync reads with --from0 --files-from. A
// NUL cannot by definition occur in a file name, so that separation is
// watertight — the same reasoning as "call external programs with an argument
// list", applied to a list too long for the command line.
func WriteRestore(log *slog.Logger, g RestoreInput) (RestoreResult, error) {
	res := RestoreResult{Reasons: map[string]int{}}
	if g.ScriptName == "" || g.ListName == "" {
		return res, errors.New("restore input without file names; the ArtifactSlot of this run was not passed on")
	}

	scriptPath, err := safeFilePath(g.Dir, g.ScriptName)
	if err != nil {
		return res, err
	}
	listPath, err := safeFilePath(g.Dir, g.ListName)
	if err != nil {
		return res, err
	}

	// The directory has to exist before writeAtomic creates its temporary file;
	// os.CreateTemp does not create directories. WriteReport does the same
	// thing, but it only runs after us — and precisely on the run where the
	// restore script is needed most (the first with deletions on a host where
	// the directory is missing) it would be the only thing absent.
	if err := os.MkdirAll(g.Dir, modeDir); err != nil {
		return res, fmt.Errorf("directory %s for the restore script cannot be created: %w", g.Dir, err)
	}

	// Validate first, write second. If one value does not fit, there is *no*
	// script and *no* list; the report explains why and gives the manual rsync
	// example.
	var bad []string
	for what, value := range scriptValues(g, scriptPath, listPath) {
		if !scriptValueRegexp.MatchString(value) {
			bad = append(bad, what)
		}
	}
	if len(bad) > 0 {
		sortStrings(bad)
		res.Message = "no restore script written: " + strings.Join(bad, " and ") +
			" contains characters that are not allowed in a generated script (allowed: letters, digits and . _ - /)"
		log.Error("restore script not written because a value did not pass validation",
			"values", bad, "allowed", scriptValueRegexp.String(),
			"consequence", "the report gives the manual rsync example instead")
		return res, nil
	}

	// The list file: paths relative to the mountpoint, unedited (this is the one
	// place where a decoded path may go unfiltered), NUL separated.
	var list []byte
	for _, v := range g.Deleted {
		if !v.Decodable {
			res.Reasons["path not decodable"]++
			continue
		}
		rel, ok := relativeWithin(g.Mountpoint, v.Path)
		if !ok || rel == "" {
			res.Reasons["path falls outside the mountpoint"]++
			continue
		}
		sourcePath, err := pathInSnapshot(SnapshotSource{Mountpoint: g.Mountpoint, Dir: g.SnapshotDir}, v.Path)
		if err != nil {
			res.Reasons["path falls outside the snapshot directory"]++
			continue
		}
		// Lstat and not Stat: we do not follow a symlink in the snapshot, and a
		// file that is not in it cannot be restored either.
		if _, err := os.Lstat(sourcePath); err != nil {
			res.Reasons["not present in the snapshot"]++
			continue
		}
		list = append(list, []byte(rel)...)
		list = append(list, 0)
		res.Lines++
	}

	if res.Lines == 0 {
		res.Message = "no restore script written: not a single vanished file could be found in the snapshot"
		log.Warn("restore script not written: no path passed the checks",
			"snapshot_dir", g.SnapshotDir, "reasons", res.Reasons,
			"check", "whether the snapshot directory can be opened (ls on <mountpoint>/.zfs/snapshot/) and whether the paths fall under the mountpoint")
		return res, nil
	}

	if err := writeAtomic(log, listPath, modeList, list); err != nil {
		return res, fmt.Errorf("list file for the restore cannot be written: %w", err)
	}
	res.ListPath = listPath

	script := buildRestoreScript(g, scriptPath, listPath, res.Lines)
	if err := writeAtomic(log, scriptPath, modeScript, []byte(script)); err != nil {
		return res, fmt.Errorf("restore script cannot be written: %w", err)
	}
	res.ScriptPath = scriptPath
	log.Info("restore script written", "path", scriptPath, "list", listPath, "lines", res.Lines)
	return res, nil
}

// scriptValues is the complete list of values that end up literally in the
// generated script, with a name that is understandable in an error message.
// Every value buildRestoreScript interpolates belongs here — that is the whole
// safeguard, and it is only as strong as it is complete.
//
// scriptPath was not part of it at first. That was not exploitable in practice
// (scriptPath and listPath come from the same g.Dir and differ only in the
// extension, so what one rejects the other rejects too), but that is a
// distraction and not a safeguard: if the naming ever changes, the check on
// that one value silently disappears.
func scriptValues(g RestoreInput, scriptPath, listPath string) map[string]string {
	return map[string]string{
		"the mountpoint of the dataset": g.Mountpoint,
		"the dataset name":              g.Dataset,
		"the snapshot name":             g.SnapshotShort,
		"the snapshot directory":        g.SnapshotDir,
		"the path to the script":        scriptPath,
		"the path to the list file":     listPath,
		"the path to zfs":               g.ZfsPath,
	}
}

// buildRestoreScript assembles the fixed text. All interpolated values were
// validated above and sit between single quotes; not a single file name appears
// in it.
func buildRestoreScript(g RestoreInput, scriptPath, listPath string, lines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/bash\n")
	fmt.Fprintf(&b, "# photowatch — restore of %s\n", g.Now.Format("2006-01-02"))
	if g.DryRun {
		// Whoever finds this script in the dry-run directory must know it comes
		// from a trial run: no snapshot was made on that run and nothing was
		// reported. The script itself does work — it draws from the existing
		// snapshot.
		b.WriteString("# This script comes from a dry run (photowatch -dry-run). It works, but\n" +
			"# it belongs to a run that reported nothing; the real script of the day\n" +
			"# is one directory up.\n")
	}
	b.WriteString("#\n")
	fmt.Fprintf(&b, "# On %s %s disappeared from the archive (%s).\n",
		g.Now.Format("2006-01-02"),
		plural(len(g.Deleted), "1 file", "%d files"),
		plural(g.MediaFiles, "1 photo", "%d photos"))
	fmt.Fprintf(&b, "# They are still in the ZFS snapshot of %s, through %s.\n#\n",
		g.PreviousTime.Format("2006-01-02"), g.RecoverUntil.Format("2006-01-02"))
	fmt.Fprintf(&b, "# What to do:\n")
	fmt.Fprintf(&b, "#   1. Log in as root on the host that holds the ZFS pool.\n")
	fmt.Fprintf(&b, "#   2. See what would happen:  bash %s\n", scriptPath)
	fmt.Fprintf(&b, "#   3. Really restore:         bash %s --apply\n", scriptPath)
	// No fourth step: Immich sees a restored file right away, without a library
	// scan. Writing the scan down as a mandatory step would make it a
	// precondition, and it is not — it is the nudge for the case where it does
	// not happen by itself. See docs/RECOVERY.md, which leads on this.
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# Then check in Immich whether the photo is back. Usually it is there\n")
	fmt.Fprintf(&b, "# right away. If not, Administration > Libraries > Scan all is the nudge.\n#\n")
	fmt.Fprintf(&b, "# This script can only CREATE files. What is there now is never\n")
	fmt.Fprintf(&b, "# overwritten and never deleted (--ignore-existing, no --delete), and\n")
	fmt.Fprintf(&b, "# existing directories keep their owner, mode and date (--no-implied-dirs).\n")
	fmt.Fprintf(&b, "# To get only part of it back, remove lines from the list — the file\n")
	fmt.Fprintf(&b, "# names sit NUL separated in %s.\n", listPath)
	// The difference between this number and the count above is the number of
	// files that cannot be restored: paths that are not decodable and files that
	// are not (or no longer) in the snapshot either. Whoever sees that
	// difference knows at once that the report has more lines than this script.
	fmt.Fprintf(&b, "# The list file holds %s.\n",
		plural(lines, "1 path", "%d paths"))
	b.WriteString("set -euo pipefail\n\n")

	fmt.Fprintf(&b, "DATASET='%s'\n", g.Dataset)
	fmt.Fprintf(&b, "SNAPSHORT='%s'\n", g.SnapshotShort)
	b.WriteString("# The full snapshot name is assembled here instead of interpolated:\n" +
		"# the @ is not among the characters photowatch allows in a generated\n" +
		"# script, and that set stays narrow rather than convenient.\n")
	b.WriteString("SNAP=\"$DATASET@$SNAPSHORT\"\n")
	fmt.Fprintf(&b, "SNAPDIR='%s'\n", g.SnapshotDir)
	fmt.Fprintf(&b, "TARGET='%s'\n", g.Mountpoint)
	fmt.Fprintf(&b, "LIST='%s'\n", listPath)
	fmt.Fprintf(&b, "ZFS='%s'\n\n", g.ZfsPath)

	b.WriteString(`apply=no
case "${1-}" in
  --apply) apply=yes ;;
  "")      apply=no ;;
  *)       echo "Usage: $0 [--apply]   (without --apply nothing is changed)" >&2; exit 2 ;;
esac

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "This script must run as root, on the storage host itself." >&2
  exit 1
fi
if ! "$ZFS" list -t snapshot -H -o name "$SNAP" >/dev/null 2>&1; then
  echo "The snapshot $SNAP no longer exists. There is nothing to restore:" >&2
  echo "the retention period has passed or the snapshot was deleted by hand." >&2
  exit 1
fi
if [[ ! -d $SNAPDIR ]]; then
  echo "The snapshot directory $SNAPDIR cannot be opened." >&2
  echo "Try 'ls \"$SNAPDIR\"' — ZFS only mounts it once you ask for it." >&2
  exit 1
fi
if [[ ! -r $LIST ]]; then
  echo "The list file $LIST is missing; without it this script does not know what to restore." >&2
  exit 1
fi
if ! command -v rsync >/dev/null 2>&1; then
  echo "rsync is not on this machine: 'apt install rsync' and try again." >&2
  exit 1
fi
if [[ ! -d $TARGET ]]; then
  echo "The target directory $TARGET does not exist; is the dataset still mounted?" >&2
  exit 1
fi

count=$(tr -cd '\0' < "$LIST" | wc -c)
echo "Snapshot: $SNAP"
echo "From:     $SNAPDIR"
echo "To:       $TARGET"
echo "Files:    $count"
if [[ $apply == no ]]; then
  echo
  echo "DRY RUN — nothing is changed. Run with --apply to really restore."
fi
echo

# An argument array and not a glued-together command string, and the file names
# through --files-from: that way no name ever passes a shell.
# --files-from implies --relative, so the paths from the list are recreated
# under $TARGET, including the directories in between.
#
# --no-implied-dirs is the flag that makes good on the promise "this script can
# only create". Without it, --relative carries over the owner, group, mode and
# mtime of every intermediate directory from the snapshot. A directory that is
# still simply there would get the permissions of fourteen days ago back, and a
# changed mtime can confuse Immich's library scan. --ignore-existing does not
# protect against that: it only covers files.
# What it costs: a directory that had disappeared as well is recreated with
# default permissions (root, umask) instead of with its old owner. That is the
# good side of this trade — permissions of a new directory can be repaired, a
# silently reverted permission change goes unnoticed.
# --omit-dir-times is the second safeguard: today the list holds only files and
# all directories are therefore 'implied', but if a directory ever ends up in
# the list, the mtime of the target is left alone then too.
args=( -aHAX --ignore-existing --no-implied-dirs --omit-dir-times
       --from0 --files-from="$LIST"
       --itemize-changes --info=stats2 )
# Deliberately an if and not '[[ ... ]] && args+=( -n )': the latter yields exit
# status 1 with --apply, and then 'set -e' stops the script without doing
# anything.
if [[ $apply == no ]]; then
  args+=( -n )
fi
rsync "${args[@]}" -- "$SNAPDIR/" "$TARGET/"

echo
if [[ $apply == no ]]; then
  echo "This was a dry run. If the list above is right, run the same with --apply."
else
  echo "Done. Check in Immich whether the photos are back; usually Immich sees them right away."
  echo "If not, Administration > Libraries > Scan all is the nudge."
fi
`)
	return b.String()
}

// plural picks between a singular and a plural text. Purely for the readability
// of the script header: "there are 1 files gone" reads like a computer error,
// and this script is read exactly when you need some confidence.
func plural(n int, one, more string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(more, n)
}

// sortStrings keeps the enumeration of rejected values stable; a map has no
// order and an error message that reads differently every run is hard to
// follow.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// SnapshotDirFor builds the path to the contents of a snapshot. It exists as a
// function so that main, the report and the restore script are guaranteed to
// use the same path.
func SnapshotDirFor(mountpoint, snapshotShort string) string {
	return filepath.Join(mountpoint, ".zfs", "snapshot", snapshotShort)
}
