// Command photowatch takes a daily ZFS snapshot of a photo dataset, counts how
// many files have disappeared since the previous snapshot and reports that to
// Home Assistant — *every* day, also when nothing is gone. That daily heartbeat
// is half the point: it is how you notice when the watch itself falls silent.
package main

import (
	"os"

	"github.com/Forsskieken/photowatch/internal/watch"
)

func main() {
	os.Exit(watch.Run())
}
