package main

import (
	"fmt"
	"regexp"
	"time"
)

// The three guardrails on deleting snapshots. This is the only irreversible
// thing the program does, so they live together here and all three are tested.
const (
	// Guardrail 2: how many snapshots always stay, no matter how old. If the
	// system clock jumps forward — after a dead BIOS battery or a wrong time
	// zone — everything suddenly looks too old. This rule makes sure there is
	// still a reference left to diff against.
	MinimumKept = 3

	// Guardrail 3: how many may go per run at most. With a larger backlog it
	// cleans up over several days, with a WARN. That way one wrong assumption
	// can never cost the whole history in one go.
	MaxDeletePerRun = 5
)

// SnapshotsToDelete picks the snapshots that may go. The returned list is
// ordered oldest first and holds full names (dataset@name).
//
// backlog is true when there were more candidates than MaxDeletePerRun; the
// caller logs that as a WARN, because the cleanup is then running behind and it
// is useful to know why the pool is not shrinking.
func SnapshotsToDelete(snaps []Snapshot, dataset, snapshotPrefix string, keepDays int, now time.Time) (gone []string, backlog bool) {
	pattern := ownNamePattern(dataset, snapshotPrefix)

	// Guardrail 1: only names that match our pattern exactly. Anything else
	// stays, even when it is older — it may belong to another snapshotter or to
	// a human.
	var own []Snapshot
	for _, s := range snaps {
		if pattern.MatchString(s.Name) {
			own = append(own, s)
		}
	}

	SortNewestFirst(own)
	if len(own) <= MinimumKept {
		return nil, false
	}
	candidates := own[MinimumKept:]

	limit := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	var tooOld []string
	for _, s := range candidates {
		if s.Created.Before(limit) {
			tooOld = append(tooOld, s.Name)
		}
	}
	if len(tooOld) == 0 {
		return nil, false
	}

	// tooOld is newest first; reverse it so the oldest goes first. That is the
	// order you want when the run breaks off halfway.
	for i, j := 0, len(tooOld)-1; i < j; i, j = i+1, j-1 {
		tooOld[i], tooOld[j] = tooOld[j], tooOld[i]
	}

	if len(tooOld) > MaxDeletePerRun {
		return tooOld[:MaxDeletePerRun], true
	}
	return tooOld, false
}

// ownNamePattern builds ^<dataset>@<prefix>-YYYY-MM-DD$. Both parts are quoted:
// a dot or a dash in a dataset name is otherwise a regexp character, and then
// the pattern would match more widely than intended.
func ownNamePattern(dataset, snapshotPrefix string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`^%s@%s-\d{4}-\d{2}-\d{2}$`,
		regexp.QuoteMeta(dataset), regexp.QuoteMeta(snapshotPrefix)))
}

// SnapshotNameFor returns the name this run would create.
func SnapshotNameFor(dataset, snapshotPrefix string, day time.Time) string {
	return fmt.Sprintf("%s@%s-%s", dataset, snapshotPrefix, day.Format("2006-01-02"))
}
