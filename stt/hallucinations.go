package stt

import "strings"

// knownHallucinations are phrases the Whisper model frequently produces when
// the audio is silent, noisy, or truncated. They originate from YouTube-style
// captions in Whisper's training data rather than real speech.
// Reference: https://github.com/openai/whisper/discussions/928
var knownHallucinations = []string{
	// Chinese (Traditional / Simplified)
	"字幕由Amara.org社區提供",
	"字幕由 Amara.org 社區提供",
	"字幕由Amara.org社区提供",
	"字幕由 Amara.org 社区提供",
	"請不吝點讚 訂閱 轉發 打賞支持明鏡與點點欄目",
	"请不吝点赞 订阅 转发 打赏支持明镜与点点栏目",
	"請訂閱我的頻道",
	"请订阅我的频道",
	"感謝您的收看",
	"感谢您的收看",
	"多謝觀看",
	"多谢观看",
	"字幕志願者",
	"字幕志愿者",

	// English
	"Thanks for watching!",
	"Thanks for watching.",
	"Thank you for watching!",
	"Thank you for watching.",
	"Please subscribe to my channel",

	// Japanese
	"ご視聴ありがとうございました。",
	"ご視聴ありがとうございました",
	"ご視聴ありがとうございます",

	// Korean
	"시청해주셔서 감사합니다.",
	"시청해주셔서 감사합니다",
	"MBC 뉴스",
}

// filterHallucinations strips known Whisper hallucination phrases from the
// transcribed text and trims surrounding whitespace. If the original text is
// entirely composed of hallucinations, the returned string will be empty.
func filterHallucinations(text string) string {
	cleaned := text
	for _, phrase := range knownHallucinations {
		cleaned = strings.ReplaceAll(cleaned, phrase, "")
	}
	return strings.TrimSpace(cleaned)
}
