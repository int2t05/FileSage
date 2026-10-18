package office

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOfficeFormatSupportsOnlyWordAndPowerPoint(t *testing.T) {
	for _, extension := range []string{".docx", ".DOCX", ".pptx", ".PPTX"} {
		if _, ok := officeFormat(extension); !ok {
			t.Errorf("officeFormat(%q) was rejected", extension)
		}
	}
	for _, extension := range []string{".doc", ".xlsx", ".pdf", ""} {
		if _, ok := officeFormat(extension); ok {
			t.Errorf("officeFormat(%q) was accepted", extension)
		}
	}
}

func TestValidateOfficePackageRequiresMatchingOOXMLContent(t *testing.T) {
	docx := officeZip(t, "word/document.xml")
	if err := validateOfficePackage(docx, officeFormatMust(t, ".docx")); err != nil {
		t.Fatalf("valid DOCX rejected: %v", err)
	}
	if err := validateOfficePackage(docx, officeFormatMust(t, ".pptx")); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("DOCX accepted as PPTX: %v", err)
	}
	if err := validateOfficePackage([]byte("not a zip"), officeFormatMust(t, ".docx")); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("invalid archive error = %v, want invalid document", err)
	}
}

func TestValidateOfficePackageRejectsUnsafeMembersAndExpandedSize(t *testing.T) {
	unsafe := officeZip(t, "../word/document.xml")
	if err := validateOfficePackage(unsafe, officeFormatMust(t, ".docx")); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("unsafe archive error = %v, want invalid document", err)
	}
	large := officeZipWithDeclaredMember(t, "word/document.xml", MaxExpandedBytes+1)
	if err := validateOfficePackage(large, officeFormatMust(t, ".docx")); !errors.Is(err, ErrExpandedTooLarge) {
		t.Fatalf("expanded archive error = %v, want expansion limit", err)
	}
}

func TestValidateOfficePackageChecksCRCAndActualExpansionLimit(t *testing.T) {
	corrupt := append([]byte(nil), officeZip(t, "word/document.xml")...)
	reader, err := zip.NewReader(bytes.NewReader(corrupt), int64(len(corrupt)))
	if err != nil {
		t.Fatal(err)
	}
	dataOffset, err := reader.File[1].DataOffset()
	if err != nil {
		t.Fatal(err)
	}
	corrupt[dataOffset] ^= 0xff
	if err := validateOfficePackage(corrupt, officeFormatMust(t, ".docx")); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("CRC mismatch error = %v, want invalid document", err)
	}
	if count, err := readOfficeEntry(context.Background(), strings.NewReader("012345"), 4); !errors.Is(err, ErrExpandedTooLarge) || count != 5 {
		t.Fatalf("actual expansion check = (%d, %v), want limit after 5 bytes", count, err)
	}
}

func TestOfficeArchiveEntryPreflightIgnoresDeclaredCount(t *testing.T) {
	archive := officeZipWithEntries(t, maxOfficeEntries+1)
	end := officeEndRecord(t, archive)
	binary.LittleEndian.PutUint16(archive[end+8:end+10], 1)
	binary.LittleEndian.PutUint16(archive[end+10:end+12], 1)
	if err := preflightOfficeArchive(archive); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("archive with spoofed small entry count error = %v, want invalid document", err)
	}
}

func TestOfficeArchiveEntryPreflightRejectsExcessiveDeclaredCount(t *testing.T) {
	archive := officeZip(t, "word/document.xml")
	end := officeEndRecord(t, archive)
	binary.LittleEndian.PutUint16(archive[end+8:end+10], maxOfficeEntries+1)
	binary.LittleEndian.PutUint16(archive[end+10:end+12], maxOfficeEntries+1)
	if err := preflightOfficeArchive(archive); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("archive with excessive declared entry count error = %v, want invalid document", err)
	}
}

func TestOfficeArchivePreflightRejectsInvalidTrailingEOCDCandidate(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "invalid disk number",
			mutate: func(candidate []byte) {
				binary.LittleEndian.PutUint16(candidate[4:6], 1)
			},
		},
		{
			name: "truncated comment",
			mutate: func(candidate []byte) {
				binary.LittleEndian.PutUint16(candidate[20:22], 1)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := officeZip(t, "word/document.xml")
			candidate := make([]byte, 22)
			binary.LittleEndian.PutUint32(candidate[:4], zipEndSignature)
			test.mutate(candidate)
			archive = append(archive, candidate...)
			if err := preflightOfficeArchive(archive); !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("archive with invalid trailing EOCD candidate error = %v, want invalid document", err)
			}
		})
	}
}

func TestOfficeArchivePreflightSupportsFixedZIP64EndRecord(t *testing.T) {
	archive := officeZIP64(t, nil)
	if err := validateOfficePackage(archive, officeFormatMust(t, ".docx")); err != nil {
		t.Fatalf("fixed ZIP64 archive rejected: %v", err)
	}
}

func TestOfficeArchivePreflightRejectsZIP64ExtensionData(t *testing.T) {
	archive := officeZIP64(t, []byte{0xaa})
	if err := preflightOfficeArchive(archive); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("ZIP64 extensible data sector error = %v, want invalid document", err)
	}
}

func TestConverterRejectsMissingDependenciesAndOversizedInput(t *testing.T) {
	if _, err := newConverter(executionPaths{}, converterOptions{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing dependencies error = %v, want unavailable", err)
	}
	converter, err := newConverter(executionPaths{oosplash: "/oosplash", programDir: "/usr/lib/libreoffice/program", bwrap: "/bwrap", prlimit: "/prlimit"}, converterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := converter.Convert(context.Background(), make([]byte, MaxInputBytes+1), ".docx"); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("oversized input error = %v, want input limit", err)
	}
}

func TestPrepareWorkDirUsesPrivatePermissions(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkDir(workDir, officeZip(t, "word/document.xml"), "source.docx"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(workDir, "source.docx"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("input permissions = %o, want owner-only", info.Mode().Perm())
	}
}

func officeZip(t *testing.T, contentName string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"[Content_Types].xml", contentName} {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, "<xml/>"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func officeZipWithEntries(t *testing.T, count int) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for i := range count {
		if _, err := writer.Create(fmt.Sprintf("entry-%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func officeEndRecord(t *testing.T, archive []byte) int {
	t.Helper()
	for position := len(archive) - 22; position >= max(0, len(archive)-65*1024); position-- {
		if binary.LittleEndian.Uint32(archive[position:position+4]) == zipEndSignature {
			return position
		}
	}
	t.Fatal("ZIP end record not found")
	return 0
}

func officeZIP64(t *testing.T, extension []byte) []byte {
	t.Helper()
	archive := officeZip(t, "word/document.xml")
	end := officeEndRecord(t, archive)
	count := binary.LittleEndian.Uint16(archive[end+10 : end+12])
	directorySize := binary.LittleEndian.Uint32(archive[end+12 : end+16])
	directoryOffset := binary.LittleEndian.Uint32(archive[end+16 : end+20])
	archive = archive[:end]
	zip64Position := uint64(len(archive))

	zip64End := make([]byte, 56+len(extension))
	binary.LittleEndian.PutUint32(zip64End[:4], zip64EndSignature)
	binary.LittleEndian.PutUint64(zip64End[4:12], uint64(44+len(extension)))
	binary.LittleEndian.PutUint16(zip64End[12:14], 45)
	binary.LittleEndian.PutUint16(zip64End[14:16], 45)
	binary.LittleEndian.PutUint64(zip64End[24:32], uint64(count))
	binary.LittleEndian.PutUint64(zip64End[32:40], uint64(count))
	binary.LittleEndian.PutUint64(zip64End[40:48], uint64(directorySize))
	binary.LittleEndian.PutUint64(zip64End[48:56], uint64(directoryOffset))
	copy(zip64End[56:], extension)
	archive = append(archive, zip64End...)

	locator := make([]byte, 20)
	binary.LittleEndian.PutUint32(locator[:4], zip64LocatorSignature)
	binary.LittleEndian.PutUint64(locator[8:16], zip64Position)
	binary.LittleEndian.PutUint32(locator[16:20], 1)
	archive = append(archive, locator...)

	classicEnd := make([]byte, 22)
	binary.LittleEndian.PutUint32(classicEnd[:4], zipEndSignature)
	binary.LittleEndian.PutUint16(classicEnd[8:10], 0xffff)
	binary.LittleEndian.PutUint16(classicEnd[10:12], 0xffff)
	binary.LittleEndian.PutUint32(classicEnd[12:16], 0xffffffff)
	binary.LittleEndian.PutUint32(classicEnd[16:20], 0xffffffff)
	return append(archive, classicEnd...)
}

func officeZipWithDeclaredMember(t *testing.T, contentName string, expanded uint64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"[Content_Types].xml", contentName} {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		if name == contentName {
			header.CRC32 = 0
			header.CompressedSize64 = 1
			header.UncompressedSize64 = expanded
			header.Flags |= 0x8
			raw, err := writer.CreateRaw(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Write([]byte{0}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, "<xml/>"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func officeFormatMust(t *testing.T, extension string) officeFormatInfo {
	t.Helper()
	format, ok := officeFormat(extension)
	if !ok {
		t.Fatalf("unsupported test format %q", extension)
	}
	return format
}
