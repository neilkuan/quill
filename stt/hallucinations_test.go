package stt

import "testing"

func TestFilterHallucinations(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "real speech is preserved",
			in:   "你好，這是一段測試語音",
			want: "你好，這是一段測試語音",
		},
		{
			name: "strips Amara hallucination",
			in:   "字幕由 Amara.org 社區提供",
			want: "",
		},
		{
			name: "strips Amara hallucination without spaces",
			in:   "字幕由Amara.org社區提供",
			want: "",
		},
		{
			name: "strips simplified Chinese Amara",
			in:   "字幕由 Amara.org 社区提供",
			want: "",
		},
		{
			name: "strips明鏡打賞 phrase",
			in:   "請不吝點讚 訂閱 轉發 打賞支持明鏡與點點欄目",
			want: "",
		},
		{
			name: "strips English Thanks for watching",
			in:   "Thanks for watching!",
			want: "",
		},
		{
			name: "strips Japanese closing phrase",
			in:   "ご視聴ありがとうございました",
			want: "",
		},
		{
			name: "strips Korean closing phrase",
			in:   "시청해주셔서 감사합니다",
			want: "",
		},
		{
			name: "keeps real speech when hallucination appended",
			in:   "今天天氣很好。Thanks for watching!",
			want: "今天天氣很好。",
		},
		{
			name: "keeps real speech when hallucination prepended",
			in:   "字幕由 Amara.org 社區提供 這是一段對話",
			want: "這是一段對話",
		},
		{
			name: "trims surrounding whitespace",
			in:   "   hello world   ",
			want: "hello world",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterHallucinations(tc.in)
			if got != tc.want {
				t.Errorf("filterHallucinations(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
