// 本文件从增量 JSON 响应中安全提取助手回复文本。
package agent

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	replyStringOther = iota
	replyStringKey
	replyStringValue
)

// replyFieldStreamer 只从结构化模型响应中提取顶层 reply 字段。
type replyFieldStreamer struct {
	depth        int
	started      bool
	inString     bool
	escaped      bool
	stringRole   int
	keyToken     []byte
	currentKey   string
	expectingKey bool
	replyRaw     []byte
	emitted      int
	replyDone    bool
}

func (stream *replyFieldStreamer) Feed(chunk string) string {
	for index := 0; index < len(chunk); index++ {
		value := chunk[index]
		if !stream.started {
			if value == '{' {
				stream.started = true
				stream.depth = 1
				stream.expectingKey = true
			}
			continue
		}
		if stream.inString {
			stream.feedStringByte(value)
			continue
		}
		switch value {
		case '"':
			stream.inString = true
			stream.escaped = false
			switch {
			case stream.depth == 1 && stream.expectingKey:
				stream.stringRole = replyStringKey
				stream.keyToken = append(stream.keyToken[:0], value)
			case stream.depth == 1 && stream.currentKey == "reply":
				stream.stringRole = replyStringValue
				stream.currentKey = ""
				stream.replyRaw = stream.replyRaw[:0]
				stream.emitted = 0
				stream.replyDone = false
			default:
				stream.stringRole = replyStringOther
			}
		case '{', '[':
			stream.depth++
		case '}', ']':
			stream.depth--
		case ',':
			if stream.depth == 1 {
				stream.expectingKey = true
				stream.currentKey = ""
			}
		case ':':
			if stream.depth == 1 {
				stream.expectingKey = false
			}
		}
	}

	decoded := decodeJSONReply(stream.replyRaw, stream.replyDone)
	if len(decoded) <= stream.emitted {
		return ""
	}
	delta := decoded[stream.emitted:]
	stream.emitted = len(decoded)
	return delta
}

func (stream *replyFieldStreamer) feedStringByte(value byte) {
	if stream.stringRole == replyStringKey {
		stream.keyToken = append(stream.keyToken, value)
	} else if stream.stringRole == replyStringValue {
		if value == '"' && !stream.escaped {
			stream.inString = false
			stream.stringRole = replyStringOther
			stream.replyDone = true
			return
		}
		stream.replyRaw = append(stream.replyRaw, value)
	} else if value == '"' && !stream.escaped {
		stream.inString = false
		stream.stringRole = replyStringOther
		return
	}
	if stream.stringRole == replyStringKey && value == '"' && !stream.escaped {
		stream.inString = false
		var key string
		if len(stream.keyToken) > 1 && unmarshalJSONString(stream.keyToken, &key) == nil {
			stream.currentKey = key
			stream.expectingKey = false
		}
		stream.stringRole = replyStringOther
		return
	}
	if value == '\\' && !stream.escaped {
		stream.escaped = true
	} else {
		stream.escaped = false
	}
}

func unmarshalJSONString(token []byte, target *string) error {
	return json.Unmarshal(token, target)
}

func decodeJSONReply(raw []byte, complete bool) string {
	var decoded strings.Builder
	for index := 0; index < len(raw); {
		value := raw[index]
		if value == '\\' {
			if index+1 >= len(raw) {
				break
			}
			switch raw[index+1] {
			case '"', '\\', '/':
				decoded.WriteByte(raw[index+1])
				index += 2
			case 'b':
				decoded.WriteByte('\b')
				index += 2
			case 'f':
				decoded.WriteByte('\f')
				index += 2
			case 'n':
				decoded.WriteByte('\n')
				index += 2
			case 'r':
				decoded.WriteByte('\r')
				index += 2
			case 't':
				decoded.WriteByte('\t')
				index += 2
			case 'u':
				if index+6 > len(raw) {
					return decoded.String()
				}
				first, err := strconv.ParseUint(string(raw[index+2:index+6]), 16, 16)
				if err != nil {
					return decoded.String()
				}
				unit := uint16(first)
				if unit >= 0xd800 && unit <= 0xdbff {
					if index+12 > len(raw) {
						if complete {
							decoded.WriteRune(utf8.RuneError)
							index += 6
							continue
						}
						return decoded.String()
					}
					if raw[index+6] == '\\' && raw[index+7] == 'u' {
						second, parseErr := strconv.ParseUint(string(raw[index+8:index+12]), 16, 16)
						if parseErr == nil && second >= 0xdc00 && second <= 0xdfff {
							decoded.WriteRune(utf16.Decode([]uint16{unit, uint16(second)})[0])
							index += 12
							continue
						}
					}
					decoded.WriteRune(utf8.RuneError)
					index += 6
					continue
				}
				decoded.WriteRune(utf16.Decode([]uint16{unit})[0])
				index += 6
			default:
				return decoded.String()
			}
			continue
		}
		if value < utf8.RuneSelf {
			decoded.WriteByte(value)
			index++
			continue
		}
		if !utf8.FullRune(raw[index:]) {
			break
		}
		runeValue, size := utf8.DecodeRune(raw[index:])
		if runeValue == utf8.RuneError && size == 1 {
			return decoded.String()
		}
		decoded.Write(raw[index : index+size])
		index += size
	}
	return decoded.String()
}
