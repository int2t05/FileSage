//go:build !linux

package office

import (
	"context"
	"fmt"
)

func privateOfficeTempRoot() (string, error) {
	return "", ErrUnavailable
}

func discoverPaths() (executionPaths, error) {
	return executionPaths{}, ErrUnavailable
}

func runConverter(context.Context, executionPaths, string, officeFormatInfo) error {
	return fmt.Errorf("%w: Bubblewrap is supported only on Linux", ErrUnavailable)
}
