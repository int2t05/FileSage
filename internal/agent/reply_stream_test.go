// 本文件验证流式回复只输出 JSON 中的助手文本。
package agent

import "testing"

func TestReplyFieldStreamerOnlyEmitsDecodedTopLevelReply(t *testing.T) {
	var stream replyFieldStreamer
	var output string
	for _, chunk := range []string{
		`{"state":"clarifying","contextSummary":"不要显示","reply":"请补`,
		`充\n路径：`,
		`\uD83D`,
		`\uDE80"}`,
	} {
		output += stream.Feed(chunk)
	}
	if output != "请补充\n路径：🚀" {
		t.Fatalf("streamed reply = %q", output)
	}
}

func TestReplyFieldStreamerWaitsForCompleteUTF8AndEscapes(t *testing.T) {
	var stream replyFieldStreamer
	output := stream.Feed(`{"reply":"café \`)
	output += stream.Feed(`"x\""}`)
	if output != `café "x"` {
		t.Fatalf("streamed reply = %q", output)
	}
}
