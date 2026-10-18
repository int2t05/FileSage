//go:build !linux

package documenttext

import "context"

func extractPDF(context.Context, []byte, bool) ([]byte, error) {
	return nil, ErrUnavailable
}

func cleanupStaleTemporaryData() error { return nil }
