package tts

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"
)

// GeminiConfig holds configuration for the Gemini TTS API.
type GeminiConfig struct {
	APIKey       string
	Model        string // default: "gemini-3.1-flash-tts-preview"
	Voice        string // prebuilt voice name (default: "Kore")
	Instructions string // Voice style/tone instructions (used as system instruction)
	Style        string // Predefined voice style: vocal_smile, newscaster, whisper, empathetic, promo_hype, deadpan
	StylePrefix  string // Emotion tag prepended to each line, e.g. "[shy]"
	StyleSuffix  string // Emotion tag appended to each line, e.g. "[laughs softly]"
	TimeoutSec   int
}

// voiceStyles maps predefined style names to their system instruction descriptions.
var voiceStyles = map[string]string{
	"vocal_smile": `Use "Vocal Smile" technique: raise the soft palate to keep the tone bright, sunny, and explicitly inviting.`,
	"newscaster":  `Speak in a professional, authoritative manner with clear articulation and standard broadcast cadence.`,
	"whisper":     `Speak in an intimate, breathy whisper with a close-to-mic proximity effect.`,
	"empathetic":  `Speak in a warm, understanding, soft tone with gentle inflections.`,
	"promo_hype":  `Speak with high energy, punchy consonants, and elongated vowels on excitement words.`,
	"deadpan":     `Speak with flat affect, minimal pitch variation, and dry delivery.`,
}

// GeminiSynthesizer uses the Google Gemini API for text-to-speech.
type GeminiSynthesizer struct {
	config GeminiConfig
}

// NewGeminiSynthesizer creates a new Gemini TTS synthesizer.
func NewGeminiSynthesizer(cfg GeminiConfig) *GeminiSynthesizer {
	if cfg.Model == "" {
		cfg.Model = "gemini-3.1-flash-tts-preview"
	}
	if cfg.Voice == "" {
		cfg.Voice = "Kore"
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 60
	}
	return &GeminiSynthesizer{config: cfg}
}

// Synthesize generates audio from text using the Gemini TTS API.
func (g *GeminiSynthesizer) Synthesize(text string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(g.config.TimeoutSec)*time.Second)
	defer cancel()

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: g.config.APIKey,
	})
	if err != nil {
		return "", fmt.Errorf("create genai client: %w", err)
	}

	text = g.applyStyleTags(text)

	contents := []*genai.Content{
		{
			Role: "user",
			Parts: []*genai.Part{
				{Text: text},
			},
		},
	}

	config := &genai.GenerateContentConfig{
		ResponseModalities: []string{"AUDIO"},
		SpeechConfig: &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: g.config.Voice,
				},
			},
		},
	}
	if sysInst := g.buildSystemInstruction(); sysInst != "" {
		config.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{
				{Text: sysInst},
			},
		}
	}

	var audioData []byte
	var mimeType string

	for resp, err := range client.Models.GenerateContentStream(ctx, g.config.Model, contents, config) {
		if err != nil {
			return "", fmt.Errorf("gemini stream error: %w", err)
		}
		if resp == nil || len(resp.Candidates) == 0 {
			continue
		}
		for _, part := range resp.Candidates[0].Content.Parts {
			if part.InlineData != nil && len(part.InlineData.Data) > 0 {
				audioData = append(audioData, part.InlineData.Data...)
				if mimeType == "" {
					mimeType = part.InlineData.MIMEType
				}
			}
		}
	}

	if len(audioData) == 0 {
		return "", fmt.Errorf("gemini returned no audio data")
	}

	bitsPerSample, sampleRate := parseAudioMimeType(mimeType)
	wavData := buildWavFile(audioData, bitsPerSample, sampleRate)

	tmpDir := os.TempDir()
	localName := fmt.Sprintf("tts_%d.wav", time.Now().UnixMilli())
	localPath := filepath.Join(tmpDir, localName)

	if err := os.WriteFile(localPath, wavData, 0600); err != nil {
		return "", fmt.Errorf("write wav file: %w", err)
	}

	return localPath, nil
}

// buildSystemInstruction combines the predefined style and custom instructions.
func (g *GeminiSynthesizer) buildSystemInstruction() string {
	var parts []string
	if desc, ok := voiceStyles[strings.ToLower(g.config.Style)]; ok {
		parts = append(parts, desc)
	}
	if g.config.Instructions != "" {
		parts = append(parts, g.config.Instructions)
	}
	return strings.Join(parts, " ")
}

// applyStyleTags wraps each non-empty line with prefix/suffix emotion tags.
// e.g. prefix="[shy]" suffix="[laughs softly]" turns "Hello" into "[shy] Hello [laughs softly]"
func (g *GeminiSynthesizer) applyStyleTags(text string) string {
	if g.config.StylePrefix == "" && g.config.StyleSuffix == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if g.config.StylePrefix != "" {
			trimmed = g.config.StylePrefix + " " + trimmed
		}
		if g.config.StyleSuffix != "" {
			trimmed = trimmed + " " + g.config.StyleSuffix
		}
		lines[i] = trimmed
	}
	return strings.Join(lines, "\n")
}

// parseAudioMimeType extracts bits per sample and sample rate from a MIME type.
// Example: "audio/L16;rate=24000" → 16, 24000
func parseAudioMimeType(mime string) (bitsPerSample, sampleRate int) {
	bitsPerSample = 16
	sampleRate = 24000

	parts := strings.Split(mime, ";")
	for _, param := range parts {
		param = strings.TrimSpace(param)
		if strings.HasPrefix(strings.ToLower(param), "rate=") {
			if v, err := strconv.Atoi(strings.SplitN(param, "=", 2)[1]); err == nil {
				sampleRate = v
			}
		} else if strings.HasPrefix(param, "audio/L") {
			if v, err := strconv.Atoi(strings.TrimPrefix(param, "audio/L")); err == nil {
				bitsPerSample = v
			}
		}
	}
	return
}

// buildWavFile prepends a WAV header to raw PCM audio data.
func buildWavFile(pcmData []byte, bitsPerSample, sampleRate int) []byte {
	numChannels := 1
	bytesPerSample := bitsPerSample / 8
	blockAlign := numChannels * bytesPerSample
	byteRate := sampleRate * blockAlign
	dataSize := len(pcmData)
	chunkSize := 36 + dataSize

	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(chunkSize))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16) // Subchunk1Size (PCM)
	binary.LittleEndian.PutUint16(header[20:22], 1)  // AudioFormat (PCM)
	binary.LittleEndian.PutUint16(header[22:24], uint16(numChannels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], uint16(bitsPerSample))
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))

	return append(header, pcmData...)
}

// Verify interface satisfaction.
var _ Synthesizer = (*GeminiSynthesizer)(nil)
