//go:build !linux

package fsroot

func platformUpdateMetadata(_ *Root, _ string, _ Fingerprint, _ MetadataUpdate) (Entry, error) {
	return Entry{}, ErrMetadataUnsupported
}
