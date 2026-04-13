// Package markdown provides a channel-agnostic pipeline for adapting LLM
// markdown output to chat platforms that don't render every GFM construct.
//
// Today it focuses on tables: Discord and Telegram both ignore GFM table
// syntax, so raw `| col | col |` lines render as a wall of pipe characters.
// ConvertTables parses the input with goldmark (AST, not regex), locates
// table nodes, and rewrites them according to the configured TableMode.
package markdown

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// codeBlockWidthThreshold is the max display column width a fenced code-block
// table may occupy before ConvertTables falls back to bullets mode. Discord
// and Telegram desktop chat panes show roughly 70-100 monospace columns; 80
// is a safe choice that keeps tables on one line for most viewports.
const codeBlockWidthThreshold = 80

// TableMode controls how GFM tables are rewritten before being sent to a chat platform.
type TableMode string

const (
	// TableModeCode wraps each table in a fenced ``` block with column-aligned
	// monospace text so chat clients render it readably.
	TableModeCode TableMode = "code"
	// TableModeBullets converts each row into a bullet list of "Header: Value" pairs.
	TableModeBullets TableMode = "bullets"
	// TableModeOff disables conversion; the original text is returned unchanged.
	TableModeOff TableMode = "off"
)

// ParseMode parses a TOML / config string into a TableMode, defaulting to
// TableModeCode when the input is empty or unrecognized.
func ParseMode(s string) TableMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bullets":
		return TableModeBullets
	case "off", "none", "disabled":
		return TableModeOff
	case "code", "":
		return TableModeCode
	default:
		return TableModeCode
	}
}

// md is shared so we don't re-build the parser on every message.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// ConvertTables rewrites GFM tables in `input` according to `mode`. Non-table
// content is returned verbatim — line endings, code blocks, indentation, and
// trailing whitespace are preserved.
func ConvertTables(input string, mode TableMode) string {
	if mode == TableModeOff || input == "" {
		return input
	}
	src := []byte(input)
	root := md.Parser().Parse(text.NewReader(src))

	type replacement struct {
		start, end int
		text       string
	}
	var repls []replacement

	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		tbl, ok := n.(*extast.Table)
		if !ok {
			return ast.WalkContinue, nil
		}
		start, end, ok := nodeByteRange(tbl, src)
		if !ok {
			return ast.WalkSkipChildren, nil
		}
		var rendered string
		switch mode {
		case TableModeBullets:
			rendered = renderBullets(tbl, src)
		default:
			rendered = renderCodeBlock(tbl, src)
			// Chat panes don't word-wrap fenced code blocks; for tables wider
			// than the viewport (common with multi-column Chinese content),
			// fall back to bullets so users don't have to horizontal-scroll.
			if maxLineWidth(rendered) > codeBlockWidthThreshold {
				rendered = renderBullets(tbl, src)
			}
		}
		repls = append(repls, replacement{start: start, end: end, text: rendered})
		return ast.WalkSkipChildren, nil
	})

	if len(repls) == 0 {
		return input
	}

	var out bytes.Buffer
	out.Grow(len(src))
	cursor := 0
	for _, r := range repls {
		if r.start < cursor {
			continue
		}
		out.Write(src[cursor:r.start])
		out.WriteString(r.text)
		cursor = r.end
	}
	out.Write(src[cursor:])
	return out.String()
}

// nodeByteRange returns the [start, end) byte offsets in src that the table
// node occupies, derived from its child rows' Segment metadata. Goldmark
// stores per-line segments on the leaf inline children, so we union them.
func nodeByteRange(tbl *extast.Table, src []byte) (int, int, bool) {
	start, end := -1, -1
	var visit func(ast.Node)
	visit = func(n ast.Node) {
		if seg, ok := segmentOf(n); ok {
			if start == -1 || seg.Start < start {
				start = seg.Start
			}
			if seg.Stop > end {
				end = seg.Stop
			}
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			visit(c)
		}
	}
	visit(tbl)
	if start == -1 || end == -1 {
		return 0, 0, false
	}
	// Extend `start` back to the beginning of the line so we replace the whole
	// `| header |` row, not just from the first inline cell content.
	for start > 0 && src[start-1] != '\n' {
		start--
	}
	// Extend `end` forward to the end of the line so trailing `|` and newline
	// belong to the replacement.
	for end < len(src) && src[end] != '\n' {
		end++
	}
	return start, end, true
}

func segmentOf(n ast.Node) (text.Segment, bool) {
	switch v := n.(type) {
	case *ast.Text:
		return v.Segment, true
	case *ast.AutoLink:
		// AutoLink also carries a Segment field via its Value method; goldmark
		// only exposes the URL bytes, so fall back to children.
		return text.Segment{}, false
	default:
		_ = v
		return text.Segment{}, false
	}
}

// --- Renderers ---

// renderCodeBlock reflows a table into an aligned monospace fenced block.
func renderCodeBlock(tbl *extast.Table, src []byte) string {
	rows := collectRows(tbl, src)
	if len(rows) == 0 {
		return ""
	}
	widths := columnWidths(rows)

	var b strings.Builder
	b.WriteString("```\n")
	for i, row := range rows {
		for j, cell := range row {
			if j > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(padRight(cell, widths[j]))
		}
		b.WriteByte('\n')
		if i == 0 && len(rows) > 1 {
			// Header divider, e.g. "----- | -----"
			for j, w := range widths {
				if j > 0 {
					b.WriteString("-+-")
				}
				b.WriteString(strings.Repeat("-", w))
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("```")
	return b.String()
}

// renderBullets emits "• Header: Value" per cell, grouped by row.
func renderBullets(tbl *extast.Table, src []byte) string {
	rows := collectRows(tbl, src)
	if len(rows) == 0 {
		return ""
	}
	headers := rows[0]
	body := rows
	if len(rows) > 1 {
		body = rows[1:]
	} else {
		// Header-only table: just print the headers as a bullet line.
		var only strings.Builder
		for i, h := range headers {
			if i > 0 {
				only.WriteString(", ")
			}
			only.WriteString(h)
		}
		return "• " + only.String()
	}

	var b strings.Builder
	for i, row := range body {
		if i > 0 {
			// Blank line between rows so users can see at a glance which
			// bullets belong to the same record. Without this every row
			// runs together and a 6-row × 4-col table becomes 24
			// indistinguishable bullets.
			b.WriteString("\n\n")
		}
		for j, cell := range row {
			if j > 0 {
				b.WriteByte('\n')
			}
			label := ""
			if j < len(headers) {
				label = headers[j]
			}
			if label != "" {
				b.WriteString(fmt.Sprintf("• %s: %s", label, cell))
			} else {
				b.WriteString("• " + cell)
			}
		}
	}
	return b.String()
}

// collectRows extracts every row's cell text from a table node, preserving
// row order (header first, then body) and trimming surrounding whitespace.
func collectRows(tbl *extast.Table, src []byte) [][]string {
	var rows [][]string
	for child := tbl.FirstChild(); child != nil; child = child.NextSibling() {
		switch row := child.(type) {
		case *extast.TableHeader:
			rows = append(rows, cellsOf(row, src))
		case *extast.TableRow:
			rows = append(rows, cellsOf(row, src))
		}
	}
	return rows
}

func cellsOf(row ast.Node, src []byte) []string {
	var cells []string
	for c := row.FirstChild(); c != nil; c = c.NextSibling() {
		cell, ok := c.(*extast.TableCell)
		if !ok {
			continue
		}
		cells = append(cells, strings.TrimSpace(inlineText(cell, src)))
	}
	return cells
}

// inlineText concatenates the literal text of all inline descendants. We use
// raw segment bytes so emphasis/code spans keep their markup characters.
func inlineText(n ast.Node, src []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(src))
		case *ast.CodeSpan:
			b.WriteString(inlineText(v, src))
		case *ast.Emphasis:
			b.WriteString(inlineText(v, src))
		case *ast.Link:
			label := inlineText(v, src)
			if label != "" {
				b.WriteString(label)
			} else {
				b.Write(v.Destination)
			}
		default:
			b.WriteString(inlineText(v, src))
		}
	}
	return b.String()
}

func columnWidths(rows [][]string) []int {
	max := 0
	for _, r := range rows {
		if len(r) > max {
			max = len(r)
		}
	}
	widths := make([]int, max)
	for _, r := range rows {
		for j, cell := range r {
			w := runewidth.StringWidth(cell)
			if w > widths[j] {
				widths[j] = w
			}
		}
	}
	return widths
}

// padRight pads s with spaces so its display width (East Asian width aware,
// so a CJK ideograph counts as 2) reaches `width`. Critical for monospace
// alignment when cells mix ASCII and Chinese.
func padRight(s string, width int) string {
	pad := width - runewidth.StringWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// maxLineWidth returns the widest line's display width in `s`, fenced
// triple-backtick markers excluded so the fence itself doesn't trigger
// the overflow fallback.
func maxLineWidth(s string) int {
	max := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "```") {
			continue
		}
		if w := runewidth.StringWidth(line); w > max {
			max = w
		}
	}
	return max
}
