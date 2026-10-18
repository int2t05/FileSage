// diff.go 提供跨平台、限长的文本差异预览。
package shellstage

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// FormatTextDiff 返回限长差异及截断标记；二进制只返回提示，不输出原文。
func FormatTextDiff(before, after []byte, maxBytes int64) (string, bool) {
	if maxBytes <= 0 {
		return "", true
	}
	if isBinaryText(before) || isBinaryText(after) {
		return boundedMessage("[binary content omitted]\n", maxBytes)
	}
	beforeTruncated, afterTruncated := int64(len(before)) > maxBytes, int64(len(after)) > maxBytes
	if beforeTruncated {
		before = before[:int(maxBytes)]
	}
	if afterTruncated {
		after = after[:int(maxBytes)]
	}
	return formatTextDiff(before, after, maxBytes, beforeTruncated, afterTruncated)
}

// formatTextDiff 生成受单文件预算限制的替换差异。
func formatTextDiff(before, after []byte, limit int64, beforeTruncated, afterTruncated bool) (string, bool) {
	var output strings.Builder
	truncated := beforeTruncated || afterTruncated
	appendPart := func(value string) bool {
		if limit < 0 || int64(output.Len()+len(value)) > limit {
			truncated = true
			return false
		}
		output.WriteString(value)
		return true
	}
	for _, header := range []string{"--- before\n", "+++ after\n", "@@\n"} {
		if !appendPart(header) {
			return output.String(), true
		}
	}
	oldLines, newLines := splitLines(before), splitLines(after)
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	contextStart := prefix - 3
	if contextStart < 0 {
		contextStart = 0
	}
	for _, line := range oldLines[contextStart:prefix] {
		if !appendPart(" " + line + "\n") {
			return appendTruncation(output.String(), limit), true
		}
	}
	for _, line := range oldLines[prefix : len(oldLines)-suffix] {
		if !appendPart("-" + line + "\n") {
			return appendTruncation(output.String(), limit), true
		}
	}
	for _, line := range newLines[prefix : len(newLines)-suffix] {
		if !appendPart("+" + line + "\n") {
			return appendTruncation(output.String(), limit), true
		}
	}
	end := len(oldLines) - suffix + 3
	if end > len(oldLines) {
		end = len(oldLines)
	}
	for _, line := range oldLines[len(oldLines)-suffix : end] {
		if !appendPart(" " + line + "\n") {
			return appendTruncation(output.String(), limit), true
		}
	}
	if !beforeTruncated && len(before) > 0 && !bytes.HasSuffix(before, []byte{'\n'}) {
		if !appendPart("\\ No newline at end of before\n") {
			return appendTruncation(output.String(), limit), true
		}
	}
	if !afterTruncated && len(after) > 0 && !bytes.HasSuffix(after, []byte{'\n'}) {
		if !appendPart("\\ No newline at end of after\n") {
			return appendTruncation(output.String(), limit), true
		}
	}
	if truncated {
		return appendTruncation(output.String(), limit), true
	}
	return output.String(), false
}

// splitLines 去掉末尾换行产生的空元素，保留空文件语义。
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// appendTruncation 在输出预算允许时追加可见截断标记。
func appendTruncation(value string, limit int64) string {
	const marker = "[diff truncated]\n"
	if limit >= 0 && int64(len(value)+len(marker)) <= limit {
		return value + marker
	}
	return value
}

// isBinaryText 拒绝含 NUL 或非法 UTF-8 的内容。
func isBinaryText(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

// boundedMessage 让状态提示也遵守输出预算。
func boundedMessage(message string, limit int64) (string, bool) {
	if limit <= 0 {
		return "", len(message) > 0
	}
	if int64(len(message)) <= limit {
		return message, false
	}
	return message[:int(limit)], true
}
