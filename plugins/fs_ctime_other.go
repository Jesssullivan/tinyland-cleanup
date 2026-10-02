//go:build !darwin && !linux

package plugins

import (
	"os"
	"time"
)

// changeTime reports no ctime on platforms where it is not exposed.
func changeTime(os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
