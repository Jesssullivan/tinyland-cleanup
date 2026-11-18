//go:build !linux

package plugins

// indexSocketListeners is a no-op off Linux. On Darwin the lsof listing in
// the activity inventory already names a bound unix socket's path.
func indexSocketListeners(*bbActivity, string, []string) {}

func retentionSocketTable() string { return "" }
