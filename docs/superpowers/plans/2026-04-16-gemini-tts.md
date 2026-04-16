# Gemini TTS & Voice Command Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Google Gemini as a TTS provider and remove the unused custom voice system (VoiceStore, CreateVoice, setvoice, voice-clear, voicemode, echo mode).

**Architecture:** Simplify the `Synthesizer` interface to a single `Synthesize` method. Add `GeminiSynthesizer` using `google.golang.org/genai` SDK's `GenerateContentStream` with WAV output. Route via `provider` field in config (mirrors existing STT pattern).

**Tech Stack:** Go, `google.golang.org/genai` v1.54.0, existing `net/http` OpenAI client

---

### Task 1: Simplify `Synthesizer` Interface & Delete Voice Files

**Files:**
- Modify: `tts/synthesizer.go` (remove `SynthesizeWithVoice`, `CreateVoice`)
- Delete: `tts/voices.go`
- Delete: `tts/voices_test.go`

- [ ] **Step 1: Simplify the `Synthesizer` interface**

Replace `tts/synthesizer.go` with:

```go
package tts

// Synthesizer converts text to an audio file.
type Synthesizer interface {
	// Synthesize generates an audio file from text and returns the local file path.
	// The caller is responsible for removing the file after use.
	Synthesize(text string) (audioPath string, err error)
}
```

- [ ] **Step 2: Delete voice store files**

```bash
rm tts/voices.go tts/voices_test.go
```

- [ ] **Step 3: Verify it doesn't compile yet (expected — dependents still reference removed methods)**

```bash
go build ./tts/...
```

Expected: SUCCESS (tts package itself should compile, since openai.go still references removed interface methods — we fix that next)

Actually this will fail because `openai.go` still has `SynthesizeWithVoice` and `CreateVoice` methods and the interface check `var _ Synthesizer = (*OpenAISynthesizer)(nil)` will fail. We fix this in Task 2.

- [ ] **Step 4: Commit**

```bash
git add tts/synthesizer.go
git rm tts/voices.go tts/voices_test.go
git commit -m "refactor(tts): simplify Synthesizer interface, remove VoiceStore"
```

---

### Task 2: Clean Up `OpenAISynthesizer`

**Files:**
- Modify: `tts/openai.go` (remove `SynthesizeWithVoice`, `CreateVoice`, `customVoiceRef`, `createVoiceResponse`, multipart upload code)
- Modify: `tts/openai_test.go` (remove tests for deleted methods)

- [ ] **Step 1: Strip `openai.go` of removed methods**

Remove from `tts/openai.go`:
- The `customVoiceRef` struct (lines 61-63)
- The `createVoiceResponse` struct (lines 66-71)
- The `SynthesizeWithVoice` method (lines 79-81)
- The `CreateVoice` method (lines 114-175) — entire multipart upload block
- In `synthesize()` method: remove the custom voice ID detection (`strings.HasPrefix(v, "voice_")` / `customVoiceRef` wrapping). The voice field is always the configured default string.

The simplified `synthesize` method:

```go
func (s *OpenAISynthesizer) synthesize(text string) (string, error) {
	req := speechRequest{
		Model:        s.config.Model,
		Input:        text,
		Voice:        s.config.Voice,
		Instructions: s.config.Instructions,
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(s.config.BaseURL, "/") + "/audio/speech"
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.config.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	return s.saveResponse(httpReq, "mp3")
}
```

Update `speechRequest` — `Voice` becomes plain `string`:

```go
type speechRequest struct {
	Model        string `json:"model"`
	Input        string `json:"input"`
	Voice        string `json:"voice"`
	Instructions string `json:"instructions,omitempty"`
}
```

Update `Synthesize` to call the simplified `synthesize`:

```go
func (s *OpenAISynthesizer) Synthesize(text string) (string, error) {
	return s.synthesize(text)
}
```

Remove unused imports: `"io"`, `"mime/multipart"`, `"os"`, `"path/filepath"`.

- [ ] **Step 2: Update `openai_test.go` — remove deleted method tests**

Remove these test functions:
- `TestOpenAISynthesizer_SynthesizeWithVoice_CustomID`
- `TestOpenAISynthesizer_SynthesizeWithVoice_BuiltIn`
- `TestOpenAISynthesizer_CreateVoice_Success`

Also remove the `createVoiceResponse` reference in the test file. The remaining tests (`TestOpenAISynthesizer_Synthesize_Success`, `TestOpenAISynthesizer_Synthesize_APIError`, `TestNewOpenAISynthesizer_Defaults`, and the interface check) stay.

Update the `Synthesize_Success` test: the voice assertion `req.Voice != "nova"` now checks against a plain string field, which it already does — no change needed.

- [ ] **Step 3: Verify tts package compiles and tests pass**

```bash
go build ./tts/... && go test ./tts/... -v
```

Expected: all remaining tests pass.

- [ ] **Step 4: Commit**

```bash
git add tts/openai.go tts/openai_test.go
git commit -m "refactor(tts): remove CreateVoice, SynthesizeWithVoice from OpenAISynthesizer"
```

---

### Task 3: Remove Voice Commands from `command/` Package

**Files:**
- Modify: `command/command.go` (remove voice command constants, remove `CustomVoice` from `VoiceInfo`)

- [ ] **Step 1: Remove voice command constants and references**

In `command/command.go`:

Remove constants:
```go
CmdSetVoice   = "setvoice"
CmdVoiceClear = "voice-clear"
CmdVoiceMode  = "voicemode"
```

Remove from `ParseCommand`'s known map:
```go
CmdSetVoice: true, CmdVoiceClear: true, CmdVoiceMode: true,
```

Remove `CustomVoice` field from `VoiceInfo`:
```go
type VoiceInfo struct {
	STTEnabled  bool
	STTProvider string
	STTModel    string
	TTSEnabled  bool
	TTSModel    string
	TTSVoice    string
}
```

Remove custom voice display from `ExecuteInfo`:
```go
// Change this:
voiceDisplay := voice.TTSVoice
if voice.CustomVoice != "" {
	voiceDisplay = fmt.Sprintf("%s (custom: `%s`)", voice.TTSVoice, voice.CustomVoice)
}
sb.WriteString(fmt.Sprintf("TTS: `%s` / %s", voice.TTSModel, voiceDisplay))

// To this:
sb.WriteString(fmt.Sprintf("TTS: `%s` / %s", voice.TTSModel, voice.TTSVoice))
```

- [ ] **Step 2: Verify command package compiles**

```bash
go build ./command/...
```

Expected: SUCCESS

- [ ] **Step 3: Commit**

```bash
git add command/command.go
git commit -m "refactor(command): remove voice commands (setvoice, voice-clear, voicemode)"
```

---

### Task 4: Remove Voice Features from Discord Adapter

**Files:**
- Modify: `discord/adapter.go` (remove `voiceStore` param)
- Modify: `discord/handler.go` (remove `VoiceStore` field, voice command handlers, custom voice logic in `sendVoiceReply` and `buildVoiceInfo`, slash command registrations, `setvoice` handling in `OnMessageCreate`)

- [ ] **Step 1: Update `discord/adapter.go`**

Change `NewAdapter` signature — remove `voiceStore *tts.VoiceStore` parameter:

```go
func NewAdapter(cfg config.DiscordConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig) (*Adapter, error) {
```

Remove `VoiceStore: voiceStore,` from the `Handler` init. Remove the `"github.com/neilkuan/quill/tts"` import if it becomes unused (it won't — `tts.Synthesizer` is still used).

- [ ] **Step 2: Update `discord/handler.go` — remove VoiceStore field**

Remove `VoiceStore *tts.VoiceStore` from `Handler` struct.

- [ ] **Step 3: Remove voice slash command registrations**

In the `slashCommands` var (around line 406-448), remove these three entries:

```go
{
	Name:        "setvoice",
	Description: "Set your custom bot voice (attach a 3-10s audio file)",
},
{
	Name:        "voice-clear",
	Description: "Clear your custom voice, revert to default",
},
{
	Name:        "voicemode",
	Description: "Set voice reply mode: echo (use your voice) or default",
	Options: []*discordgo.ApplicationCommandOption{
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "mode",
			Description: "echo = bot uses your voice, default = normal",
			Required:    true,
			Choices: []*discordgo.ApplicationCommandOptionChoice{
				{Name: "echo", Value: "echo"},
				{Name: "default", Value: "default"},
			},
		},
	},
},
```

- [ ] **Step 4: Remove voice command cases from `OnInteractionCreate`**

Remove these cases from the switch in `OnInteractionCreate`:

```go
case command.CmdSetVoice:
	response = h.handleSetVoice(s, i, userID)
case command.CmdVoiceClear:
	response = h.handleVoiceClear(userID)
case command.CmdVoiceMode:
	mode := ""
	for _, opt := range data.Options {
		if opt.Name == "mode" {
			mode = opt.StringValue()
		}
	}
	response = h.handleVoiceMode(userID, mode)
```

- [ ] **Step 5: Remove "setvoice" handling from `OnMessageCreate`**

In the audio attachment loop (around lines 238-252), remove the entire `setvoice` command block:

```go
// Handle "setvoice" command: create custom voice via OpenAI API
if strings.EqualFold(strings.TrimSpace(prompt), "setvoice") && h.VoiceStore != nil && h.Synthesizer != nil {
	voiceID, createErr := h.Synthesizer.CreateVoice(m.Author.Username, localPath)
	os.Remove(localPath)
	if createErr != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⚠️ Failed to create voice: %v", createErr))
	} else {
		if sErr := h.VoiceStore.SetVoice(m.Author.ID, voiceID); sErr != nil {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⚠️ Voice created but failed to save: %v", sErr))
		} else {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("✅ Your custom voice has been created! (ID: `%s`)", voiceID))
		}
	}
	return
}
```

- [ ] **Step 6: Simplify `sendVoiceReply`**

Replace the entire `sendVoiceReply` method with:

```go
func (h *Handler) sendVoiceReply(s *discordgo.Session, channelID, userID, text string) {
	slog.Info("🔊 tts: synthesizing voice reply", "user", userID, "voice", h.TTSConfig.Voice, "text_length", len(text))
	audioPath, err := h.Synthesizer.Synthesize(text)
	if err != nil {
		slog.Error("tts synthesis failed", "error", err)
		return
	}
	defer os.Remove(audioPath)

	f, err := os.Open(audioPath)
	if err != nil {
		slog.Error("failed to open tts audio", "error", err)
		return
	}
	defer f.Close()

	if _, err := s.ChannelFileSend(channelID, "voice_reply.mp3", f); err != nil {
		slog.Error("failed to send tts voice", "error", err)
	}
}
```

- [ ] **Step 7: Simplify `buildVoiceInfo`**

Replace with:

```go
func (h *Handler) buildVoiceInfo(userID string) *command.VoiceInfo {
	vi := &command.VoiceInfo{
		STTEnabled: h.Transcriber != nil,
		TTSEnabled: h.Synthesizer != nil,
		TTSModel:   h.TTSConfig.Model,
		TTSVoice:   h.TTSConfig.Voice,
	}
	if h.Transcriber != nil {
		vi.STTProvider = "openai"
	}
	return vi
}
```

- [ ] **Step 8: Delete the three voice command handler methods**

Remove entirely:
- `handleSetVoice`
- `handleVoiceClear`
- `handleVoiceMode`

- [ ] **Step 9: Clean up unused imports in `discord/handler.go`**

The `"github.com/neilkuan/quill/tts"` import may now be unused — remove if so. Keep `"fmt"`, `"os"`, etc. as needed.

- [ ] **Step 10: Verify Discord package compiles**

```bash
go build ./discord/...
```

Expected: FAIL — `main.go` still passes `voiceStore` to `discord.NewAdapter`. That's fixed in Task 6.

- [ ] **Step 11: Commit**

```bash
git add discord/adapter.go discord/handler.go
git commit -m "refactor(discord): remove voice commands and VoiceStore"
```

---

### Task 5: Remove Voice Features from Telegram Adapter

**Files:**
- Modify: `telegram/adapter.go` (remove `voiceStore` param, remove voice bot commands)
- Modify: `telegram/handler.go` (remove `VoiceStore` field, voice command handlers, custom voice logic in `sendVoiceReply` and `buildVoiceInfo`)

- [ ] **Step 1: Update `telegram/adapter.go`**

Change `NewAdapter` signature — remove `voiceStore *tts.VoiceStore`:

```go
func NewAdapter(cfg config.TelegramConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig) (*Adapter, error) {
```

Remove `VoiceStore: voiceStore,` from the `Handler` init.

Remove voice bot commands from `SetMyCommands` (lines 134-136):

```go
{Command: "setvoice", Description: "Set custom bot voice (reply to a voice message)"},
{Command: "voice_clear", Description: "Clear your custom voice"},
{Command: "voicemode", Description: "Set voice mode: echo or default"},
```

- [ ] **Step 2: Update `telegram/handler.go` — remove VoiceStore field**

Remove `VoiceStore *tts.VoiceStore` from `Handler` struct.

- [ ] **Step 3: Remove voice command cases from the command switch**

Remove these cases (around lines 410-415):

```go
case command.CmdSetVoice:
	response = h.handleSetVoice(ctx, b, msg, userID)
case command.CmdVoiceClear:
	response = h.handleVoiceClear(userID)
case command.CmdVoiceMode:
	response = h.handleVoiceMode(userID, cmd.Args)
```

- [ ] **Step 4: Simplify `sendVoiceReply`**

Replace the voice selection logic with direct `Synthesize` call:

```go
func (h *Handler) sendVoiceReply(ctx context.Context, b *bot.Bot, chatID int64, replyToMsgID int, userID, text string) {
	slog.Info("🔊 tts: synthesizing voice reply", "user", userID, "voice", h.TTSConfig.Voice, "text_length", len(text))
	audioPath, err := h.Synthesizer.Synthesize(text)
	if err != nil {
		slog.Error("tts synthesis failed", "error", err)
		return
	}
	defer os.Remove(audioPath)
```

Keep the rest of the method (open file, SendVoice) unchanged.

- [ ] **Step 5: Simplify `buildVoiceInfo`**

Replace with:

```go
func (h *Handler) buildVoiceInfo(userID string) *command.VoiceInfo {
	vi := &command.VoiceInfo{
		STTEnabled: h.Transcriber != nil,
		TTSEnabled: h.Synthesizer != nil,
		TTSModel:   h.TTSConfig.Model,
		TTSVoice:   h.TTSConfig.Voice,
	}
	if h.Transcriber != nil {
		vi.STTProvider = "openai"
	}
	return vi
}
```

- [ ] **Step 6: Delete voice command handler methods**

Remove entirely:
- `handleSetVoice`
- `handleVoiceClear`
- `handleVoiceMode`

- [ ] **Step 7: Clean up unused imports in `telegram/handler.go`**

Remove `"github.com/neilkuan/quill/tts"` if unused. Keep `"strings"` etc. as needed.

- [ ] **Step 8: Commit**

```bash
git add telegram/adapter.go telegram/handler.go
git commit -m "refactor(telegram): remove voice commands and VoiceStore"
```

---

### Task 6: Update Config & `main.go` — Add `provider` Field, Remove VoiceStore

**Files:**
- Modify: `config/config.go` (add `Provider` field to `TTSConfig`, update defaults)
- Modify: `main.go` (provider switch for TTS, remove voiceStore creation, update adapter calls)

- [ ] **Step 1: Add `Provider` field to `TTSConfig`**

In `config/config.go`, update `TTSConfig`:

```go
type TTSConfig struct {
	Enabled     bool   `toml:"enabled"`
	Provider    string `toml:"provider"`     // "openai" or "gemini" (default: "openai")
	APIKey      string `toml:"api_key"`
	Model       string `toml:"model"`
	Voice       string `toml:"voice"`
	VoiceGender string `toml:"voice_gender"` // OpenAI only; ignored when provider != "openai"
	BaseURL     string `toml:"base_url"`     // OpenAI only
	TimeoutSec  int    `toml:"timeout_sec"`
}
```

- [ ] **Step 2: Update `applyTTSDefaults` for provider routing**

```go
func applyTTSDefaults(tc *TTSConfig) {
	if tc.APIKey != "" && !tc.Enabled {
		tc.Enabled = true
	}
	if tc.Provider == "" {
		tc.Provider = "openai"
	}
	if tc.TimeoutSec == 0 {
		tc.TimeoutSec = 60
	}

	switch tc.Provider {
	case "gemini":
		if tc.Model == "" {
			tc.Model = "gemini-3.1-flash-tts-preview"
		}
		if tc.Voice == "" {
			tc.Voice = "Kore"
		}
	default: // "openai"
		if tc.Model == "" {
			tc.Model = "tts-1"
		}
		if tc.VoiceGender == "" {
			tc.VoiceGender = "female"
		}
		if tc.Voice == "" {
			switch tc.VoiceGender {
			case "male":
				tc.Voice = "ash"
			default:
				tc.Voice = "nova"
			}
		}
	}
}
```

- [ ] **Step 3: Update `main.go` — provider switch, remove voiceStore**

Replace the TTS creation block with:

```go
// Create TTS (text-to-speech) synthesizer if configured
var synth tts.Synthesizer
if cfg.TTS.Enabled {
	switch cfg.TTS.Provider {
	case "openai":
		synth = tts.NewOpenAISynthesizer(tts.OpenAIConfig{
			APIKey:     cfg.TTS.APIKey,
			Model:      cfg.TTS.Model,
			Voice:      cfg.TTS.Voice,
			BaseURL:    cfg.TTS.BaseURL,
			TimeoutSec: cfg.TTS.TimeoutSec,
		})
	case "gemini":
		synth = tts.NewGeminiSynthesizer(tts.GeminiConfig{
			APIKey:     cfg.TTS.APIKey,
			Model:      cfg.TTS.Model,
			Voice:      cfg.TTS.Voice,
			TimeoutSec: cfg.TTS.TimeoutSec,
		})
	default:
		slog.Warn("unknown tts provider, voice synthesis disabled", "provider", cfg.TTS.Provider)
	}
	if synth != nil {
		slog.Info("🔊 tts enabled", "provider", cfg.TTS.Provider, "model", cfg.TTS.Model, "voice", cfg.TTS.Voice)
	}
}
```

Remove `var voiceStore *tts.VoiceStore` and the entire voiceStore creation block.

Update adapter constructor calls — remove `voiceStore` parameter:

```go
// Discord:
adapter, err := discord.NewAdapter(cfg.Discord, pool, t, synth, cfg.TTS, cfg.Markdown)

// Telegram:
adapter, err := telegram.NewAdapter(cfg.Telegram, pool, t, synth, cfg.TTS, cfg.Markdown)
```

- [ ] **Step 4: Verify compilation (will fail on missing `tts.NewGeminiSynthesizer` — expected)**

```bash
go build ./...
```

Expected: FAIL — `tts.NewGeminiSynthesizer` and `tts.GeminiConfig` don't exist yet. This is OK; Task 7 adds them.

- [ ] **Step 5: Commit**

```bash
git add config/config.go main.go
git commit -m "feat(config): add TTS provider field, remove VoiceStore from main"
```

---

### Task 7: Implement `GeminiSynthesizer`

**Files:**
- Create: `tts/gemini.go`
- Create: `tts/gemini_test.go`

- [ ] **Step 1: Write the WAV header test first**

Create `tts/gemini_test.go`:

```go
package tts

import (
	"encoding/binary"
	"testing"
)

func TestParseAudioMimeType(t *testing.T) {
	tests := []struct {
		mime           string
		wantBits       int
		wantRate       int
	}{
		{"audio/L16;rate=24000", 16, 24000},
		{"audio/L16;rate=16000", 16, 16000},
		{"audio/L24;rate=24000", 24, 24000},
		{"audio/L16", 16, 24000},       // default rate
		{"audio/pcm", 16, 24000},       // all defaults
		{"", 16, 24000},                // empty = all defaults
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
	pcm := make([]byte, 100) // 100 bytes of fake PCM data
	wav := buildWavFile(pcm, 16, 24000)

	// WAV header is 44 bytes
	if len(wav) != 144 {
		t.Fatalf("expected 144 bytes (44 header + 100 data), got %d", len(wav))
	}

	// Check RIFF header
	if string(wav[0:4]) != "RIFF" {
		t.Errorf("expected RIFF, got %q", string(wav[0:4]))
	}

	// ChunkSize = total - 8
	chunkSize := binary.LittleEndian.Uint32(wav[4:8])
	if chunkSize != 136 { // 144 - 8
		t.Errorf("expected ChunkSize 136, got %d", chunkSize)
	}

	// WAVE format
	if string(wav[8:12]) != "WAVE" {
		t.Errorf("expected WAVE, got %q", string(wav[8:12]))
	}

	// Sample rate at offset 24
	sampleRate := binary.LittleEndian.Uint32(wav[24:28])
	if sampleRate != 24000 {
		t.Errorf("expected sample rate 24000, got %d", sampleRate)
	}

	// Bits per sample at offset 34
	bitsPerSample := binary.LittleEndian.Uint16(wav[34:36])
	if bitsPerSample != 16 {
		t.Errorf("expected 16 bits, got %d", bitsPerSample)
	}

	// data chunk size at offset 40
	dataSize := binary.LittleEndian.Uint32(wav[40:44])
	if dataSize != 100 {
		t.Errorf("expected data size 100, got %d", dataSize)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./tts/... -run "TestParseAudioMimeType|TestBuildWavHeader" -v
```

Expected: FAIL — functions don't exist yet.

- [ ] **Step 3: Implement `GeminiSynthesizer`**

Create `tts/gemini.go`:

```go
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
	APIKey     string
	Model      string // default: "gemini-3.1-flash-tts-preview"
	Voice      string // prebuilt voice name (default: "Kore")
	TimeoutSec int
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
	chunkSize := 36 + dataSize // 36 bytes for header fields before data size

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
```

- [ ] **Step 4: Run WAV tests**

```bash
go test ./tts/... -run "TestParseAudioMimeType|TestBuildWavHeader" -v
```

Expected: PASS

- [ ] **Step 5: Run full test suite**

```bash
go build ./... && go test ./... -v
```

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add tts/gemini.go tts/gemini_test.go
git commit -m "feat(tts): add GeminiSynthesizer using google.golang.org/genai SDK"
```

---

### Task 8: Update Config Example & CLAUDE.md

**Files:**
- Modify: `config.toml.example` (add Gemini TTS section, add provider field to existing sections)
- Modify: `CLAUDE.md` (remove voice command docs, update TTS description)

- [ ] **Step 1: Update `config.toml.example`**

Replace the existing `[tts]` section with:

```toml
# TTS - Text-to-Speech (optional)
# Bot replies with voice when user sends a voice message.
# provider = "openai" (default) or "gemini"

# OpenAI TTS:
# [tts]
# provider = "openai"
# api_key = "${OPENAI_API_KEY}"
# model = "tts-1"              # or "tts-1-hd", "gpt-4o-mini-tts"
# voice = "nova"               # alloy, ash, ballad, coral, echo, fable, nova, onyx, sage, shimmer
# base_url = "https://api.openai.com/v1"

# Gemini TTS (female voice):
# [tts]
# provider = "gemini"
# api_key = "${GEMINI_API_KEY}"
# model = "gemini-3.1-flash-tts-preview"   # or gemini-2.5-flash-preview-tts, gemini-2.5-pro-preview-tts
# voice = "Kore"               # Kore (Firm, Middle pitch)

# Gemini TTS (male voice):
# [tts]
# provider = "gemini"
# api_key = "${GEMINI_API_KEY}"
# model = "gemini-3.1-flash-tts-preview"
# voice = "Orus"               # Orus (Firm, Lower middle pitch)
```

- [ ] **Step 2: Update CLAUDE.md — remove voice commands from table and descriptions**

Remove the "Voice commands" table and all references to `/setvoice`, `/voice-clear`, `/voicemode`, `VoiceStore`, `CreateVoice`, `SynthesizeWithVoice`, echo mode from the CLAUDE.md architecture docs.

Update TTS package description to mention provider pattern:

```
- **`tts/`** — Text-to-speech synthesis.
  - `synthesizer.go` — Defines `Synthesizer` interface: `Synthesize(text string) (audioPath, error)`.
  - `openai.go` — `OpenAISynthesizer` using OpenAI TTS API.
  - `gemini.go` — `GeminiSynthesizer` using Google Gemini API (`google.golang.org/genai`). Streams audio via `GenerateContentStream`, assembles raw PCM chunks into WAV files.
  - Enabled when `tts.api_key` is set in config. Provider selected via `tts.provider` field (`"openai"` default, `"gemini"`).
```

- [ ] **Step 3: Commit**

```bash
git add config.toml.example CLAUDE.md
git commit -m "docs: update config example and CLAUDE.md for Gemini TTS, remove voice commands"
```

---

### Task 9: Final Verification

- [ ] **Step 1: Run full build**

```bash
go build ./...
```

Expected: SUCCESS

- [ ] **Step 2: Run vet**

```bash
go vet ./...
```

Expected: no issues

- [ ] **Step 3: Run all tests**

```bash
go test ./... -v
```

Expected: all pass

- [ ] **Step 4: Verify go.sum is clean**

```bash
go mod tidy
```

Check if any modules were added/removed unexpectedly.

- [ ] **Step 5: Commit go.mod/go.sum if changed**

```bash
git add go.mod go.sum
git commit -m "chore: update go.mod/go.sum for google.golang.org/genai"
```
