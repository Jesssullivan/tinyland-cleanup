//go:build !linux

package plugins

import (
	"errors"
	"time"
)

// Session and working-directory inspection reads /proc; elsewhere it fails,
// which keeps every hook pin and every agent-tree root.
func nixSessionStartTimes(comms []string) ([]time.Time, error) {
	return nil, errors.New("session inspection needs /proc")
}

func nixLiveCwds(okComms []string) ([]string, error) {
	return nil, errors.New("working-directory inspection needs /proc")
}
