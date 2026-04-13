package markdown

import (
	"strings"
	"testing"
)

const sampleTable = `Here is the comparison:

| Tool | Stars | Language |
| --- | --- | --- |
| Foo | 1.2k | Go |
| Bar | 800 | Rust |

That's all.
`

func TestConvertTablesCodeMode(t *testing.T) {
	out := ConvertTables(sampleTable, TableModeCode)

	if !strings.Contains(out, "```") {
		t.Fatalf("code mode should wrap table in fenced block, got:\n%s", out)
	}
	if strings.Contains(out, "| --- |") {
		t.Fatalf("GFM divider row should be replaced, got:\n%s", out)
	}
	if !strings.Contains(out, "Tool") || !strings.Contains(out, "Foo") || !strings.Contains(out, "Bar") {
		t.Fatalf("cells missing in output:\n%s", out)
	}
	// Surrounding prose preserved.
	if !strings.Contains(out, "Here is the comparison:") || !strings.Contains(out, "That's all.") {
		t.Fatalf("surrounding prose lost:\n%s", out)
	}
}

func TestConvertTablesBulletsMode(t *testing.T) {
	out := ConvertTables(sampleTable, TableModeBullets)

	if strings.Contains(out, "|") {
		t.Fatalf("bullets mode should remove pipe characters, got:\n%s", out)
	}
	if !strings.Contains(out, "• Tool: Foo") {
		t.Fatalf("expected '• Tool: Foo' in bullet output, got:\n%s", out)
	}
	if !strings.Contains(out, "• Language: Rust") {
		t.Fatalf("expected '• Language: Rust' in bullet output, got:\n%s", out)
	}
}

func TestConvertTablesOffMode(t *testing.T) {
	out := ConvertTables(sampleTable, TableModeOff)
	if out != sampleTable {
		t.Fatalf("off mode should return input unchanged.\nwant:\n%s\ngot:\n%s", sampleTable, out)
	}
}

func TestConvertTablesPassThroughNonTable(t *testing.T) {
	in := "# Title\n\nJust text, no table here. **bold** _italic_ `code`.\n"
	out := ConvertTables(in, TableModeCode)
	if out != in {
		t.Fatalf("non-table input should be unchanged.\nwant:\n%s\ngot:\n%s", in, out)
	}
}

func TestConvertTablesPreservesCodeBlocks(t *testing.T) {
	in := "Example:\n\n```go\nfunc x() {}\n```\n\nDone.\n"
	out := ConvertTables(in, TableModeCode)
	if out != in {
		t.Fatalf("fenced code blocks must not be touched.\nwant:\n%s\ngot:\n%s", in, out)
	}
}

func TestConvertTablesMultipleTables(t *testing.T) {
	in := "First:\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\nSecond:\n\n| x | y |\n| --- | --- |\n| 9 | 8 |\n"
	out := ConvertTables(in, TableModeCode)
	if strings.Count(out, "```") != 4 { // 2 tables × open+close fences
		t.Fatalf("expected two fenced blocks (4 ``` markers), got:\n%s", out)
	}
}

func TestParseMode(t *testing.T) {
	cases := map[string]TableMode{
		"":         TableModeCode,
		"code":     TableModeCode,
		"CODE":     TableModeCode,
		"bullets":  TableModeBullets,
		"Bullets":  TableModeBullets,
		"off":      TableModeOff,
		"none":     TableModeOff,
		"disabled": TableModeOff,
		"junk":     TableModeCode,
	}
	for in, want := range cases {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertTablesEmptyInput(t *testing.T) {
	if got := ConvertTables("", TableModeCode); got != "" {
		t.Fatalf("expected empty output for empty input, got %q", got)
	}
}

// TestConvertTablesCJKAlignment verifies that CJK characters are counted as
// width 2 so columns line up in monospace fonts. Without runewidth this would
// produce a ragged table on Discord/Telegram.
func TestConvertTablesCJKAlignment(t *testing.T) {
	in := "| 名稱 | 說明 |\n| --- | --- |\n| 短 | OK |\n| 長一點的中文 | 描述 |\n"
	out := ConvertTables(in, TableModeCode)
	// Should still be code mode (narrow enough).
	if !strings.Contains(out, "```") {
		t.Fatalf("narrow CJK table should stay in code mode, got:\n%s", out)
	}
	// Each non-fence, non-divider line should have the same display width.
	var widths []int
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "```") || strings.Contains(line, "-+-") || line == "" {
			continue
		}
		if !strings.Contains(line, "|") {
			continue
		}
		widths = append(widths, displayWidth(line))
	}
	if len(widths) < 2 {
		t.Fatalf("expected multiple rows, got widths=%v\noutput:\n%s", widths, out)
	}
	for _, w := range widths[1:] {
		if w != widths[0] {
			t.Fatalf("CJK column widths misaligned: %v\noutput:\n%s", widths, out)
		}
	}
}

// TestConvertTablesWideTableFallsBackToBullets verifies the auto-fallback
// when a code-mode table would overflow chat viewport width.
func TestConvertTablesWideTableFallsBackToBullets(t *testing.T) {
	// Build a 4-column table whose rendered line will exceed 80 cols.
	in := "| col1 | col2 | col3 | col4 |\n| --- | --- | --- | --- |\n" +
		"| " + strings.Repeat("a", 25) + " | " + strings.Repeat("b", 25) + " | " +
		strings.Repeat("c", 25) + " | " + strings.Repeat("d", 25) + " |\n"
	out := ConvertTables(in, TableModeCode)
	if strings.Contains(out, "```") {
		t.Fatalf("wide table should fall back to bullets, got code block:\n%s", out)
	}
	if !strings.Contains(out, "• col1:") {
		t.Fatalf("expected bullet output, got:\n%s", out)
	}
}

// displayWidth is a tiny helper mirroring runewidth so the test stays
// independent of the implementation file's imports.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r == 0:
			// skip
		case r >= 0x1100 && r <= 0x115F,
			r >= 0x2E80 && r <= 0x303E,
			r >= 0x3041 && r <= 0x33FF,
			r >= 0x3400 && r <= 0x4DBF,
			r >= 0x4E00 && r <= 0x9FFF,
			r >= 0xA000 && r <= 0xA4CF,
			r >= 0xAC00 && r <= 0xD7A3,
			r >= 0xF900 && r <= 0xFAFF,
			r >= 0xFE30 && r <= 0xFE4F,
			r >= 0xFF00 && r <= 0xFF60,
			r >= 0xFFE0 && r <= 0xFFE6:
			w += 2
		default:
			w++
		}
	}
	return w
}
