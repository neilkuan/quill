package tts

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestParseAudioMimeType(t *testing.T) {
	tests := []struct {
		mime     string
		wantBits int
		wantRate int
	}{
		{"audio/L16;rate=24000", 16, 24000},
		{"audio/L16;rate=16000", 16, 16000},
		{"audio/L24;rate=24000", 24, 24000},
		{"audio/L16", 16, 24000},
		{"audio/pcm", 16, 24000},
		{"", 16, 24000},
	}

	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			bits, rate := parseAudioMimeType(tt.mime)
			if bits != tt.wantBits {
				t.Errorf("bits: got %d, want %d", bits, tt.wantBits)
			}
			if rate != tt.wantRate {
				t.Errorf("rate: got %d, want %d", rate, tt.wantRate)
			}
		})
	}
}

func TestBuildWavHeader(t *testing.T) {
	pcm := make([]byte, 100)
	wav := buildWavFile(pcm, 16, 24000)

	if len(wav) != 144 {
		t.Fatalf("expected 144 bytes (44 header + 100 data), got %d", len(wav))
	}

	if string(wav[0:4]) != "RIFF" {
		t.Errorf("expected RIFF, got %q", string(wav[0:4]))
	}

	chunkSize := binary.LittleEndian.Uint32(wav[4:8])
	if chunkSize != 136 {
		t.Errorf("expected ChunkSize 136, got %d", chunkSize)
	}

	if string(wav[8:12]) != "WAVE" {
		t.Errorf("expected WAVE, got %q", string(wav[8:12]))
	}

	sampleRate := binary.LittleEndian.Uint32(wav[24:28])
	if sampleRate != 24000 {
		t.Errorf("expected sample rate 24000, got %d", sampleRate)
	}

	bitsPerSample := binary.LittleEndian.Uint16(wav[34:36])
	if bitsPerSample != 16 {
		t.Errorf("expected 16 bits, got %d", bitsPerSample)
	}

	dataSize := binary.LittleEndian.Uint32(wav[40:44])
	if dataSize != 100 {
		t.Errorf("expected data size 100, got %d", dataSize)
	}
}

func TestNewGeminiSynthesizer_Defaults(t *testing.T) {
	synth := NewGeminiSynthesizer(GeminiConfig{APIKey: "test-key"})

	if synth.config.Model != "gemini-3.1-flash-tts-preview" {
		t.Errorf("expected default model, got %q", synth.config.Model)
	}
	if synth.config.Voice != "Kore" {
		t.Errorf("expected default voice 'Kore', got %q", synth.config.Voice)
	}
	if synth.config.TimeoutSec != 60 {
		t.Errorf("expected default timeout 60, got %d", synth.config.TimeoutSec)
	}
}

func TestBuildSystemInstruction(t *testing.T) {
	tests := []struct {
		name         string
		style        string
		instructions string
		wantContains string
		wantEmpty    bool
	}{
		{name: "no style no instructions", wantEmpty: true},
		{name: "style only", style: "whisper", wantContains: "intimate, breathy whisper"},
		{name: "instructions only", instructions: "Be cheerful", wantContains: "Be cheerful"},
		{name: "both combined", style: "newscaster", instructions: "Use short sentences", wantContains: "professional"},
		{name: "unknown style ignored", style: "unknown", instructions: "Custom", wantContains: "Custom"},
		{name: "case insensitive", style: "Vocal_Smile", wantContains: "bright, sunny"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GeminiSynthesizer{config: GeminiConfig{
				Style:        tt.style,
				Instructions: tt.instructions,
			}}
			got := g.buildSystemInstruction()
			if tt.wantEmpty && got != "" {
				t.Errorf("expected empty, got %q", got)
			}
			if tt.wantContains != "" && !strings.Contains(strings.ToLower(got), strings.ToLower(tt.wantContains)) {
				t.Errorf("expected to contain %q, got %q", tt.wantContains, got)
			}
		})
	}
}

func TestApplyStyleTags(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		suffix  string
		input   string
		want    string
	}{
		{
			name:  "no tags",
			input: "Hello world",
			want:  "Hello world",
		},
		{
			name:   "prefix only",
			prefix: "[shy]",
			input:  "Hello\nWorld",
			want:   "[shy] Hello\n[shy] World",
		},
		{
			name:   "suffix only",
			suffix: "[laughs softly]",
			input:  "Hello\nWorld",
			want:   "Hello [laughs softly]\nWorld [laughs softly]",
		},
		{
			name:   "both tags",
			prefix: "[shy]",
			suffix: "[laughs softly]",
			input:  "Line 1\nLine 2\nLine 3",
			want:   "[shy] Line 1 [laughs softly]\n[shy] Line 2 [laughs softly]\n[shy] Line 3 [laughs softly]",
		},
		{
			name:   "skip empty lines",
			prefix: "[shy]",
			suffix: "[laughs]",
			input:  "Hello\n\nWorld",
			want:   "[shy] Hello [laughs]\n\n[shy] World [laughs]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GeminiSynthesizer{config: GeminiConfig{
				StylePrefix: tt.prefix,
				StyleSuffix: tt.suffix,
			}}
			got := g.applyStyleTags(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

var _ Synthesizer = (*GeminiSynthesizer)(nil)
