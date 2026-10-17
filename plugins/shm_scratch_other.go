//go:build !linux

package plugins

// shmIsTmpfs is false off Linux: the shm_scratch plugin is Linux-only.
func shmIsTmpfs(string) (bool, error) { return false, nil }
