package platform

import (
	"strings"
	"testing"
)

func TestSplitMessage_ShortText(t *testing.T) {
	result := SplitMessage("hello", 2000)
	if len(result) != 1 || result[0] != "hello" {
		t.Fatalf("expected single chunk 'hello', got %v", result)
	}
}

func TestSplitMessage_ExactLimit(t *testing.T) {
	text := strings.Repeat("a", 100)
	result := SplitMessage(text, 100)
	if len(result) != 1 || result[0] != text {
		t.Fatalf("expected single chunk of length 100, got %d chunks", len(result))
	}
}

func TestSplitMessage_SplitsAtLineBoundary(t *testing.T) {
	lines := []string{
		strings.Repeat("a", 10),
		strings.Repeat("b", 10),
		strings.Repeat("c", 10),
	}
	text := strings.Join(lines, "\n")
	result := SplitMessage(text, 25)

	if len(result) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(result))
	}
	reassembled := strings.Join(result, "\n")
	if reassembled != text {
		t.Fatalf("reassembled text doesn't match original.\noriginal:    %q\nreassembled: %q", text, reassembled)
	}
}

func TestSplitMessage_HardSplitLongLine(t *testing.T) {
	line := strings.Repeat("x", 50)
	result := SplitMessage(line, 20)

	for _, chunk := range result {
		if len(chunk) > 20 {
			t.Fatalf("chunk exceeds limit: len=%d, content=%q", len(chunk), chunk)
		}
	}
	joined := strings.Join(result, "")
	if joined != line {
		t.Fatalf("hard-split chunks don't reassemble: got %q", joined)
	}
}

func TestSplitMessage_EmptyString(t *testing.T) {
	result := SplitMessage("", 100)
	if len(result) != 1 || result[0] != "" {
		t.Fatalf("expected single empty chunk, got %v", result)
	}
}

func TestSplitMessage_MultipleLines(t *testing.T) {
	text := "line1\nline2\nline3\nline4\nline5"
	result := SplitMessage(text, 2000)
	if len(result) != 1 || result[0] != text {
		t.Fatalf("expected single chunk for short multiline text, got %d chunks", len(result))
	}
}

func TestSplitMessage_AllChunksWithinLimit(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("a", 15))
	}
	text := strings.Join(lines, "\n")
	limit := 50
	result := SplitMessage(text, limit)

	for i, chunk := range result {
		if len(chunk) > limit {
			t.Fatalf("chunk %d exceeds limit %d: len=%d", i, limit, len(chunk))
		}
	}
}

func TestSplitMessage_HardSplitUTF8Safe(t *testing.T) {
	// "你好世界" = 4 chars, 12 bytes. Limit 7 bytes → must not cut mid-character.
	line := "你好世界"
	result := SplitMessage(line, 7)

	for i, chunk := range result {
		// Verify valid UTF-8 by round-tripping through []rune
		if string([]rune(chunk)) != chunk {
			t.Fatalf("chunk %d is not valid UTF-8: %q", i, chunk)
		}
		if len(chunk) > 7 {
			t.Fatalf("chunk %d exceeds limit: len=%d, content=%q", i, len(chunk), chunk)
		}
	}
	joined := strings.Join(result, "")
	if joined != line {
		t.Fatalf("chunks don't reassemble: got %q, want %q", joined, line)
	}
}

func TestTruncateUTF8_Short(t *testing.T) {
	result := TruncateUTF8("hello", 100, "…")
	if result != "hello" {
		t.Fatalf("expected 'hello', got %q", result)
	}
}

func TestTruncateUTF8_Truncates(t *testing.T) {
	result := TruncateUTF8("hello world", 8, "…")
	// "hello w" = 7 bytes + "…" = 10 bytes, but target is 8 - 3(…) = 5
	// so "hello" + "…"
	if len(result) > 8 {
		t.Fatalf("result exceeds limit: len=%d, content=%q", len(result), result)
	}
	if !strings.HasSuffix(result, "…") {
		t.Fatalf("expected suffix '…', got %q", result)
	}
}

func TestTruncateUTF8_CJK(t *testing.T) {
	text := "你好世界這是測試"
	result := TruncateUTF8(text, 10, "…")
	// Each CJK char = 3 bytes, "…" = 3 bytes, target = 10 - 3 = 7 bytes → 2 chars (6 bytes)
	if len(result) > 10 {
		t.Fatalf("result exceeds limit: len=%d, content=%q", len(result), result)
	}
	if string([]rune(result)) != result {
		t.Fatalf("result is not valid UTF-8: %q", result)
	}
}
