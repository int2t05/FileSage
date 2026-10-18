//go:build !linux

package fsroot

import (
	"context"
	"errors"
	"testing"
)

func TestWalkUnsupportedPlatformFailsExplicitly(t *testing.T) {
	err := (&Root{}).Walk(context.Background(), func(Entry) error { return nil })
	if !errors.Is(err, ErrWalkUnsupported) {
		t.Fatalf("Walk() error = %v, want ErrWalkUnsupported", err)
	}
}
