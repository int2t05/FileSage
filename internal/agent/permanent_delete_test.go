// 本文件验证不可恢复删除必须来自用户明确请求。
package agent

import (
	"testing"

	"filesage/internal/store"
)

// TestUserExplicitlyRequestedPermanentDelete 覆盖明确请求与否定表达。
func TestUserExplicitlyRequestedPermanentDelete(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{message: "请永久删除这个文件", want: true},
		{message: "这份过期数据彻底删掉，不需要恢复", want: true},
		{message: "delete this permanently", want: true},
		{message: "请把文件放进可恢复回收站", want: false},
		{message: "请删除文件，但不要永久删除", want: false},
		{message: "我没有授权永久删除这个文件", want: false},
		{message: "我不授权永久删除这个文件", want: false},
		{message: "我拒绝永久删除这个文件", want: false},
		{message: "请勿永久删除这个文件", want: false},
		{message: "是否永久删除这个文件？", want: false},
		{message: "我只是想知道永久删除后能不能恢复", want: false},
		{message: "请解释“永久删除”是什么意思", want: false},
		{message: "这个文件删除后无法恢复", want: false},
		{message: "不允许永久删除这个文件", want: false},
		{message: "不可以永久删除这个文件", want: false},
		{message: "不应永久删除这个文件", want: false},
		{message: "避免永久删除这个文件", want: false},
		{message: "do not permanently delete this", want: false},
		{message: "must not permanently delete this", want: false},
	}
	for _, test := range cases {
		t.Run(test.message, func(t *testing.T) {
			messages := []store.ConversationMessage{{Role: "user", Content: test.message}}
			if got := userExplicitlyRequestedPermanentDelete(messages); got != test.want {
				t.Fatalf("userExplicitlyRequestedPermanentDelete(%q) = %t, want %t", test.message, got, test.want)
			}
		})
	}
}

// TestPermanentDeleteIntentUsesLatestUserMessage 防止旧请求覆盖用户最新决定。
func TestPermanentDeleteIntentUsesLatestUserMessage(t *testing.T) {
	messages := []store.ConversationMessage{
		{Role: "user", Content: "永久删除这个文件"},
		{Role: "assistant", Content: "请确认"},
		{Role: "user", Content: "算了，放回收站"},
	}
	if userExplicitlyRequestedPermanentDelete(messages) {
		t.Fatal("an earlier delete request must not override the user's latest message")
	}
}
