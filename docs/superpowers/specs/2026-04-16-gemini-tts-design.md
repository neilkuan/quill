# Gemini TTS Support & Voice Command Cleanup

**Date:** 2026-04-16
**Status:** Approved

## Summary

Add Google Gemini as a TTS provider alongside the existing OpenAI TTS, and remove the unused custom voice system (`CreateVoice`, `VoiceStore`, `/setvoice`, `/voice-clear`, `/voicemode`, echo mode).

## Motivation

- Gemini Flash 3.1 TTS offers high-quality multilingual speech with natural intonation and emotion tags
- Custom voice creation via OpenAI is being deprecated; remove dead code paths
- Align TTS config with STT's existing `provider` pattern

## Scope

### In scope

- New `tts/gemini.go` — `GeminiSynthesizer` using `google.golang.org/genai` SDK
- `TTSConfig` gains a `provider` field (default `"openai"`)
- `main.go` TTS init uses `switch cfg.TTS.Provider` (mirrors STT pattern)
- Simplify `Synthesizer` interface to single `Synthesize(text string) (audioPath string, err error)`
- Remove: `CreateVoice`, `SynthesizeWithVoice`, `VoiceStore`, `voices.go`, echo mode
- Remove commands: `/setvoice`, `/voice-clear`, `/voicemode` from Discord, Telegram, and `command/` package
- Remove `voiceStore` parameter from all adapter constructors
- Update `config.toml.example`

### Out of scope

- Gemini STT (not supported by Gemini TTS models)
- Multi-speaker TTS (future work)
- Gemini Vertex AI backend (API key only for now)

## Design

### 1. Simplified `Synthesizer` Interface

```go
// tts/synthesizer.go
type Synthesizer interface {
    Synthesize(text string) (audioPath string, err error)
}
```

Remove `SynthesizeWithVoice` and `CreateVoice`.

### 2. `OpenAISynthesizer` Changes

- Remove `CreateVoice` method
- Remove `SynthesizeWithVoice` method (the internal `synthesize` helper always uses the configured default voice)
- Remove `customVoiceRef`, `createVoiceResponse` types
- Remove multipart upload code

### 3. New `GeminiSynthesizer`

File: `tts/gemini.go`

```go
type GeminiConfig struct {
    APIKey     string
    Model      string // default: "gemini-3.1-flash-tts-preview"
    Voice      string // default: "Kore" (prebuilt voice name)
    TimeoutSec int
}

type GeminiSynthesizer struct {
    config GeminiConfig
}
```

**Flow:**

1. Create `genai.Client` with API key (`genai.NewClient(ctx, &genai.ClientConfig{APIKey: ...})`)
2. Build `[]*genai.Content` with user text as a `Part`
3. Configure `GenerateContentConfig`:
   - `ResponseModalities: []string{"AUDIO"}`
   - `SpeechConfig: &genai.SpeechConfig{VoiceConfig: &genai.VoiceConfig{PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: voice}}}`
4. Call `client.Models.GenerateContentStream(ctx, model, contents, config)`
5. Iterate streaming chunks, collect `part.InlineData.Data` (raw PCM audio bytes) and `part.InlineData.MIMEType`
6. Parse MIME type to extract sample rate and bit depth (e.g. `audio/L16;rate=24000`)
7. Prepend WAV header to raw PCM data
8. Write to temp file, return path

**WAV header construction** follows the same logic as the user's Python example — `convert_to_wav()` and `parse_audio_mime_type()` ported to Go.

### 4. Config Changes

```go
// config/config.go
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

Default behavior:
- `provider` defaults to `"openai"` in `applyTTSDefaults`
- When `provider == "gemini"`: model defaults to `"gemini-3.1-flash-tts-preview"`, voice defaults to `"Kore"`
- When `provider == "openai"`: existing defaults preserved

### 5. `main.go` TTS Init

```go
switch cfg.TTS.Provider {
case "openai":
    synth = tts.NewOpenAISynthesizer(tts.OpenAIConfig{...})
case "gemini":
    synth = tts.NewGeminiSynthesizer(tts.GeminiConfig{...})
default:
    slog.Warn("unknown tts provider", "provider", cfg.TTS.Provider)
}
```

Remove `voiceStore` creation and all `voiceStore` references.

### 6. Command Removal

Remove from `command/` package:
- `setvoice` command handler
- `voice-clear` / `voice_clear` command handler
- `voicemode` command handler

Remove from `discord/handler.go`:
- Slash command registration for `/setvoice`, `/voice-clear`, `/voicemode`
- Interaction handlers
- `voiceStore` field and usage
- `SynthesizeWithVoice` calls → replace with `Synthesize`

Remove from `telegram/handler.go`:
- Bot command registration for `/setvoice`, `/voice_clear`, `/voicemode`
- Command handlers
- `voiceStore` field and usage
- `SynthesizeWithVoice` calls → replace with `Synthesize`

### 7. Files to Delete

- `tts/voices.go`
- `tts/voices_test.go`

### 8. Config Example Update

Add Gemini TTS example to `config.toml.example`:

```toml
# TTS - Text-to-Speech (optional)
# provider = "openai" or "gemini"
# [tts]
# provider = "openai"
# api_key = "${OPENAI_API_KEY}"
# model = "tts-1"
# voice = "nova"

# [tts]
# provider = "gemini"
# api_key = "${GEMINI_API_KEY}"
# model = "gemini-3.1-flash-tts-preview"
# voice = "Kore"    # female (Firm, Middle pitch)
# voice = "Orus"    # male (Firm, Lower middle pitch)
```

## Gemini Available Voices

Full list: https://ai.google.dev/gemini-api/docs/speech-generation

| Voice | Style | Pitch |
|---|---|---|
| Achernar | Soft | Higher pitch |
| Achird | Friendly | Lower middle pitch |
| Algenib | Gravelly | Lower pitch |
| Algieba | Smooth | Lower pitch |
| Alnilam | Firm | Lower middle pitch |
| Aoede | Breezy | Middle pitch |
| Autonoe | Bright | Middle pitch |
| Callirrhoe | Easy-going | Middle pitch |
| Charon | Informative | Lower pitch |
| Despina | Smooth | Middle pitch |
| Enceladus | Breathy | Lower pitch |
| Erinome | Clear | Middle pitch |
| Fenrir | Excitable | Lower middle pitch |
| Gacrux | Mature | Middle pitch |
| Iapetus | Clear | Lower middle pitch |
| **Kore** | **Firm** | **Middle pitch** |
| Laomedeia | Upbeat | Higher pitch |
| Leda | Youthful | Higher pitch |
| **Orus** | **Firm** | **Lower middle pitch** |
| Puck | Upbeat | Middle pitch |
| Pulcherrima | Forward | Middle pitch |
| Rasalgethi | Informative | Middle pitch |
| Sadachbia | Lively | Lower pitch |
| Sadaltager | Knowledgeable | Middle pitch |
| Schedar | Even | Lower middle pitch |
| Sulafat | Warm | Middle pitch |
| Umbriel | Easy-going | Lower middle pitch |
| Vindemiatrix | Gentle | Middle pitch |
| Zephyr | Bright | Higher pitch |
| Zubenelgenubi | Casual | Lower middle pitch |

Default: **Kore** (female), recommended male: **Orus**

## Testing

- Unit test for `GeminiSynthesizer` WAV header construction (`convert_to_wav`, `parse_audio_mime_type`)
- Unit test for `OpenAISynthesizer` after interface simplification
- Existing tests updated to remove `VoiceStore` / `CreateVoice` / `SynthesizeWithVoice` references
- `go vet ./...` and `go build ./...` pass

## Dependencies

- `google.golang.org/genai` (already added: v1.54.0)
