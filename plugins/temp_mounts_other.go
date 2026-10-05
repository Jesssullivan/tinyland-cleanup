//go:build !linux && !darwin

package plugins

import (
	"context"
	"fmt"
)

func nativeTemporaryMounts(context.Context) ([]string, error) {
	return nil, fmt.Errorf("authoritative mount inventory unsupported on this platform")
}
