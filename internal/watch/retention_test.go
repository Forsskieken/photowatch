package watch

import (
	"fmt"
	"testing"
	"time"
)

// makeSnaps builds n daily snapshots, oldest first, ending on end.
func makeSnaps(dataset, prefix string, n int, end time.Time) []Snapshot {
	var out []Snapshot
	for i := n - 1; i >= 0; i-- {
		day := end.AddDate(0, 0, -i)
		name := fmt.Sprintf("%s@%s-%s", dataset, prefix, day.Format("2006-01-02"))
		out = append(out, Snapshot{Name: name, Short: shortSnapName(name), Created: day})
	}
	return out
}

func contains(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

// Guardrail 1: only names matching dataset@prefix-YYYY-MM-DD exactly may go.
func TestSnapshotsToDeleteLeavesForeignNames(t *testing.T) {
	now := time.Date(2026, 8, 29, 8, 15, 0, 0, time.Local)
	old := now.AddDate(0, 0, -100)
	snaps := makeSnaps("tank/photos", "photowatch", 20, now)
	foreign := []Snapshot{
		{Name: "tank/photos@manual-before-the-cleanup", Short: "manual-before-the-cleanup", Created: old},
		{Name: "tank/photos@photowatch-2026-8-1", Short: "photowatch-2026-8-1", Created: old}, // not zero-padded
		{Name: "tank/photos@photowatch-2026-08-01-extra", Short: "photowatch-2026-08-01-extra", Created: old},
		{Name: "tank/photos@photowatch", Short: "photowatch", Created: old},                      // no date
		{Name: "tank/other@photowatch-2026-01-01", Short: "photowatch-2026-01-01", Created: old}, // other dataset
	}
	gone, _ := SnapshotsToDelete(append(snaps, foreign...), "tank/photos", "photowatch", 14, now)
	for _, f := range foreign {
		if contains(gone, f.Name) {
			t.Errorf("snapshot with a deviating name would be deleted: %s", f.Name)
		}
	}
	if len(gone) == 0 {
		t.Error("nothing was pruned while there are 20 days of our own snapshots")
	}
}

// Guardrail 2: if the clock jumps 100 days forward, three still remain.
func TestSnapshotsToDeleteAlwaysKeepsThree(t *testing.T) {
	created := time.Date(2026, 8, 29, 8, 15, 0, 0, time.Local)
	snaps := makeSnaps("tank/photos", "photowatch", 14, created)
	now := created.AddDate(0, 0, 100) // clock suddenly far ahead

	// Prune repeatedly until nothing may go: guardrail 3 makes that happen in
	// steps of at most 5 per run.
	for round := 0; round < 10; round++ {
		gone, _ := SnapshotsToDelete(snaps, "tank/photos", "photowatch", 14, now)
		if len(gone) == 0 {
			break
		}
		var left []Snapshot
		for _, s := range snaps {
			if !contains(gone, s.Name) {
				left = append(left, s)
			}
		}
		snaps = left
	}
	if len(snaps) != MinimumKept {
		t.Fatalf("%d snapshots remained, want %d: %v", len(snaps), MinimumKept, snaps)
	}
	// And they must be the newest three.
	SortNewestFirst(snaps)
	if snaps[0].Short != "photowatch-2026-08-29" {
		t.Errorf("the newest snapshot is %s; that one should have stayed", snaps[0].Short)
	}
}

// Guardrail 3: never more than MaxDeletePerRun at a time, with a signal that
// there is a backlog.
func TestSnapshotsToDeleteMaximumPerRun(t *testing.T) {
	now := time.Date(2026, 8, 29, 8, 15, 0, 0, time.Local)
	snaps := makeSnaps("tank/photos", "photowatch", 60, now)
	gone, backlog := SnapshotsToDelete(snaps, "tank/photos", "photowatch", 14, now)
	if len(gone) != MaxDeletePerRun {
		t.Fatalf("%d snapshots would go, at most %d allowed", len(gone), MaxDeletePerRun)
	}
	if !backlog {
		t.Error("backlog was not reported while there were dozens of candidates")
	}
	// The oldest must go first.
	if gone[0] != snaps[0].Name {
		t.Errorf("first deletion is %s, want the oldest (%s)", gone[0], snaps[0].Name)
	}
}

func TestSnapshotsToDeleteLeavesYoungSnapshots(t *testing.T) {
	now := time.Date(2026, 8, 29, 8, 15, 0, 0, time.Local)
	snaps := makeSnaps("tank/photos", "photowatch", 14, now) // all within 14 days
	gone, backlog := SnapshotsToDelete(snaps, "tank/photos", "photowatch", 14, now)
	if len(gone) != 0 {
		t.Errorf("%d snapshots would go while nothing is older than the retention: %v", len(gone), gone)
	}
	if backlog {
		t.Error("backlog reported while nothing had to go")
	}
}

func TestSnapshotsToDeleteWithFewSnapshots(t *testing.T) {
	now := time.Date(2026, 8, 29, 8, 15, 0, 0, time.Local)
	snaps := makeSnaps("tank/photos", "photowatch", 3, now.AddDate(0, 0, -300))
	gone, _ := SnapshotsToDelete(snaps, "tank/photos", "photowatch", 14, now)
	if len(gone) != 0 {
		t.Errorf("with only 3 ancient snapshots nothing may go, got: %v", gone)
	}
}

func TestSnapshotNameFor(t *testing.T) {
	day := time.Date(2026, 8, 29, 8, 17, 3, 0, time.Local)
	if got, want := SnapshotNameFor("tank/photos", "photowatch", day), "tank/photos@photowatch-2026-08-29"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
}
