// 本文件在 ZIP 标准库解析目录前限制 OOXML 包条目数。
package office

import "encoding/binary"

const maxOfficeEntries = 10000

const (
	zipEndSignature           = uint32(0x06054b50)
	zip64LocatorSignature     = uint32(0x07064b50)
	zip64EndSignature         = uint32(0x06064b50)
	zipCentralHeaderSignature = uint32(0x02014b50)
	zipDigitalSignature       = uint32(0x05054b50)
)

type officeDirectoryEnd struct {
	position uint64
	count    uint64
	size     uint64
	offset   uint64
}

// preflightOfficeArchive 在 ZIP 标准库分配目录前核实真实条目数。
func preflightOfficeArchive(data []byte) error {
	position, ok := findOfficeDirectoryEnd(data)
	if !ok {
		return ErrInvalidDocument
	}
	end, ok := parseOfficeDirectoryEnd(data, position)
	if !ok || end.count > maxOfficeEntries {
		return ErrInvalidDocument
	}
	return countOfficeCentralEntries(data, end.position, end.size, end.offset, end.count)
}

// findOfficeDirectoryEnd 复现 Go 标准库的 EOCD 窗口和候选选择规则。
func findOfficeDirectoryEnd(data []byte) (int, bool) {
	const recordSize = 22
	for _, windowSize := range []int{1024, 65 * 1024} {
		if windowSize > len(data) {
			windowSize = len(data)
		}
		windowStart := len(data) - windowSize
		for localPosition := windowSize - recordSize; localPosition >= 0; localPosition-- {
			position := windowStart + localPosition
			if binary.LittleEndian.Uint32(data[position:position+4]) != zipEndSignature {
				continue
			}
			commentSize := int(binary.LittleEndian.Uint16(data[position+20 : position+22]))
			if localPosition+recordSize+commentSize > windowSize {
				return 0, false
			}
			return position, true
		}
		if windowSize == len(data) {
			return 0, false
		}
	}
	return 0, false
}

// parseOfficeDirectoryEnd 读取 ZIP 结束记录及可选的 ZIP64 记录。
func parseOfficeDirectoryEnd(data []byte, position int) (officeDirectoryEnd, bool) {
	if binary.LittleEndian.Uint16(data[position+4:position+6]) != 0 ||
		binary.LittleEndian.Uint16(data[position+6:position+8]) != 0 {
		return officeDirectoryEnd{}, false
	}
	countOnDisk := binary.LittleEndian.Uint16(data[position+8 : position+10])
	count := binary.LittleEndian.Uint16(data[position+10 : position+12])
	if countOnDisk != count {
		return officeDirectoryEnd{}, false
	}
	end := officeDirectoryEnd{
		position: uint64(position),
		count:    uint64(count),
		size:     uint64(binary.LittleEndian.Uint32(data[position+12 : position+16])),
		offset:   uint64(binary.LittleEndian.Uint32(data[position+16 : position+20])),
	}
	if count == 0xffff || end.size == 0xffffffff || end.offset == 0xffffffff {
		return parseOfficeZIP64End(data, position)
	}
	return end, true
}

// parseOfficeZIP64End 读取标准布局的 ZIP64 目录结束记录。
func parseOfficeZIP64End(data []byte, endPosition int) (officeDirectoryEnd, bool) {
	const locatorSize = 20
	locatorPosition := endPosition - locatorSize
	if locatorPosition < 0 ||
		binary.LittleEndian.Uint32(data[locatorPosition:locatorPosition+4]) != zip64LocatorSignature ||
		binary.LittleEndian.Uint32(data[locatorPosition+4:locatorPosition+8]) != 0 ||
		binary.LittleEndian.Uint32(data[locatorPosition+16:locatorPosition+20]) != 1 {
		return officeDirectoryEnd{}, false
	}

	zip64Position := binary.LittleEndian.Uint64(data[locatorPosition+8 : locatorPosition+16])
	if zip64Position > uint64(len(data)) || uint64(len(data))-zip64Position < 56 || zip64Position > uint64(locatorPosition) {
		return officeDirectoryEnd{}, false
	}
	position := int(zip64Position)
	// 只接受紧邻 locator 的固定 ZIP64 结束记录，不支持扩展数据区。
	if position+56 != locatorPosition || binary.LittleEndian.Uint32(data[position:position+4]) != zip64EndSignature ||
		binary.LittleEndian.Uint64(data[position+4:position+12]) != 44 ||
		binary.LittleEndian.Uint32(data[position+16:position+20]) != 0 ||
		binary.LittleEndian.Uint32(data[position+20:position+24]) != 0 {
		return officeDirectoryEnd{}, false
	}
	countOnDisk := binary.LittleEndian.Uint64(data[position+24 : position+32])
	count := binary.LittleEndian.Uint64(data[position+32 : position+40])
	if countOnDisk != count {
		return officeDirectoryEnd{}, false
	}
	return officeDirectoryEnd{
		position: uint64(position),
		count:    count,
		size:     binary.LittleEndian.Uint64(data[position+40 : position+48]),
		offset:   binary.LittleEndian.Uint64(data[position+48 : position+56]),
	}, true
}

// countOfficeCentralEntries 按中央目录记录计数，不信任 EOCD 声明值。
func countOfficeCentralEntries(data []byte, directoryEnd, size, offset, declaredCount uint64) error {
	if directoryEnd > uint64(len(data)) || size > directoryEnd {
		return ErrInvalidDocument
	}
	start := directoryEnd - size
	if start < offset {
		return ErrInvalidDocument
	}
	position, end := start, directoryEnd
	var count uint64
	for position < end {
		remaining := end - position
		if remaining >= 6 && binary.LittleEndian.Uint32(data[int(position):int(position)+4]) == zipDigitalSignature {
			signatureSize := uint64(binary.LittleEndian.Uint16(data[int(position)+4 : int(position)+6]))
			if remaining != 6+signatureSize {
				return ErrInvalidDocument
			}
			position = end
			break
		}
		if remaining < 46 || binary.LittleEndian.Uint32(data[int(position):int(position)+4]) != zipCentralHeaderSignature {
			return ErrInvalidDocument
		}
		nameSize := uint64(binary.LittleEndian.Uint16(data[int(position)+28 : int(position)+30]))
		extraSize := uint64(binary.LittleEndian.Uint16(data[int(position)+30 : int(position)+32]))
		commentSize := uint64(binary.LittleEndian.Uint16(data[int(position)+32 : int(position)+34]))
		recordSize := uint64(46) + nameSize + extraSize + commentSize
		if recordSize > remaining {
			return ErrInvalidDocument
		}
		count++
		if count > maxOfficeEntries {
			return ErrInvalidDocument
		}
		position += recordSize
	}
	if position != end || count != declaredCount {
		return ErrInvalidDocument
	}
	return nil
}
