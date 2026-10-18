// 本文件在 ZIP 标准库解析前限制中央目录规模。
package fsroot

import (
	"encoding/binary"
	"io"
)

const (
	zipEndRecordSize        = 22
	zipMaxCommentSize       = 1<<16 - 1
	zip64LocatorSize        = 20
	zip64EndRecordSize      = 56
	zipCentralHeaderSize    = 46
	zipMaxDirectoryBytes    = 64 << 20
	zipEndSignature         = 0x06054b50
	zip64LocatorSignature   = 0x07064b50
	zip64EndSignature       = 0x06064b50
	zipCentralSignature     = 0x02014b50
	zipCentralSignSignature = 0x05054b50
)

// validateZIPDirectoryLimits 在分配条目对象前限制目录字节数和条目数。
func validateZIPDirectoryLimits(reader io.ReaderAt, size int64, maxEntries int) error {
	if reader == nil || size < zipEndRecordSize || maxEntries < 1 {
		return ErrArchiveCorrupt
	}
	tailSize := int64(zipEndRecordSize + zipMaxCommentSize)
	if tailSize > size {
		tailSize = size
	}
	tail := make([]byte, int(tailSize))
	if err := readZIPAt(reader, tail, size-tailSize); err != nil {
		return err
	}
	endIndex := -1
	for index := len(tail) - zipEndRecordSize; index >= 0; index-- {
		record := tail[index : index+zipEndRecordSize]
		if binary.LittleEndian.Uint32(record[:4]) != zipEndSignature {
			continue
		}
		commentSize := int(binary.LittleEndian.Uint16(record[20:22]))
		if index+zipEndRecordSize+commentSize <= len(tail) {
			endIndex = index
			break
		}
	}
	if endIndex < 0 {
		return ErrArchiveCorrupt
	}
	endRecord := tail[endIndex : endIndex+zipEndRecordSize]
	endOffset := size - tailSize + int64(endIndex)
	disk := uint32(binary.LittleEndian.Uint16(endRecord[4:6]))
	directoryDisk := uint32(binary.LittleEndian.Uint16(endRecord[6:8]))
	entriesOnDisk := uint64(binary.LittleEndian.Uint16(endRecord[8:10]))
	entryCount := uint64(binary.LittleEndian.Uint16(endRecord[10:12]))
	directorySize := uint64(binary.LittleEndian.Uint32(endRecord[12:16]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(endRecord[16:20]))

	if entryCount == 0xffff || directorySize == 0xffffffff || directoryOffset == 0xffffffff {
		var locator [zip64LocatorSize]byte
		if endOffset < zip64LocatorSize || readZIPAt(reader, locator[:], endOffset-zip64LocatorSize) != nil {
			return ErrArchiveCorrupt
		}
		if binary.LittleEndian.Uint32(locator[:4]) != zip64LocatorSignature {
			return ErrArchiveCorrupt
		}
		if binary.LittleEndian.Uint32(locator[4:8]) != 0 || binary.LittleEndian.Uint32(locator[16:20]) != 1 {
			return ErrArchiveUnsupportedFile
		}
		zip64Offset := binary.LittleEndian.Uint64(locator[8:16])
		if zip64Offset > uint64(size) || uint64(size)-zip64Offset < zip64EndRecordSize {
			return ErrArchiveCorrupt
		}
		var zip64End [zip64EndRecordSize]byte
		if err := readZIPAt(reader, zip64End[:], int64(zip64Offset)); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(zip64End[:4]) != zip64EndSignature ||
			binary.LittleEndian.Uint64(zip64End[4:12]) < zip64EndRecordSize-12 {
			return ErrArchiveCorrupt
		}
		disk = binary.LittleEndian.Uint32(zip64End[16:20])
		directoryDisk = binary.LittleEndian.Uint32(zip64End[20:24])
		entriesOnDisk = binary.LittleEndian.Uint64(zip64End[24:32])
		entryCount = binary.LittleEndian.Uint64(zip64End[32:40])
		directorySize = binary.LittleEndian.Uint64(zip64End[40:48])
		directoryOffset = binary.LittleEndian.Uint64(zip64End[48:56])
		endOffset = int64(zip64Offset)
	}
	if disk != 0 || directoryDisk != 0 || entriesOnDisk != entryCount {
		return ErrArchiveUnsupportedFile
	}
	if entryCount > uint64(maxEntries) || directorySize > zipMaxDirectoryBytes {
		return ErrArchiveLimitExceeded
	}
	if directorySize > uint64(endOffset) || directoryOffset > uint64(endOffset)-directorySize {
		return ErrArchiveCorrupt
	}
	directoryStart := endOffset - int64(directorySize)
	if directoryStart < 0 || directorySize > uint64(size-directoryStart) {
		return ErrArchiveCorrupt
	}

	var counted uint64
	for offset := int64(0); offset < int64(directorySize); {
		var signature [4]byte
		if err := readZIPAt(reader, signature[:], directoryStart+offset); err != nil {
			return err
		}
		switch binary.LittleEndian.Uint32(signature[:]) {
		case zipCentralSignature:
			if int64(directorySize)-offset < zipCentralHeaderSize {
				return ErrArchiveCorrupt
			}
			var header [zipCentralHeaderSize]byte
			if err := readZIPAt(reader, header[:], directoryStart+offset); err != nil {
				return err
			}
			nameSize := uint64(binary.LittleEndian.Uint16(header[28:30]))
			extraSize := uint64(binary.LittleEndian.Uint16(header[30:32]))
			commentSize := uint64(binary.LittleEndian.Uint16(header[32:34]))
			recordSize := uint64(zipCentralHeaderSize) + nameSize + extraSize + commentSize
			if nameSize > maxArchivePathBytes || recordSize > directorySize-uint64(offset) {
				return ErrArchiveLimitExceeded
			}
			counted++
			if counted > uint64(maxEntries) {
				return ErrArchiveLimitExceeded
			}
			offset += int64(recordSize)
		case zipCentralSignSignature:
			if int64(directorySize)-offset < 6 {
				return ErrArchiveCorrupt
			}
			var signatureHeader [6]byte
			if err := readZIPAt(reader, signatureHeader[:], directoryStart+offset); err != nil {
				return err
			}
			signatureSize := uint64(binary.LittleEndian.Uint16(signatureHeader[4:6]))
			if uint64(int64(directorySize)-offset) != 6+signatureSize {
				return ErrArchiveCorrupt
			}
			offset = int64(directorySize)
		default:
			return ErrArchiveCorrupt
		}
	}
	if counted != entryCount {
		return ErrArchiveCorrupt
	}
	return nil
}

func readZIPAt(reader io.ReaderAt, data []byte, offset int64) error {
	if offset < 0 {
		return ErrArchiveCorrupt
	}
	count, err := reader.ReadAt(data, offset)
	if count != len(data) || (err != nil && err != io.EOF) {
		return ErrArchiveCorrupt
	}
	return nil
}
