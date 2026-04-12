package tts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenAIConfig holds configuration for the OpenAI TTS API.
type OpenAIConfig struct {
	APIKey       string // OpenAI API key
	Model        string // "tts-1", "tts-1-hd", or "gpt-4o-mini-tts"
	Voice        string // Built-in voice name (alloy, ash, ballad, coral, echo, etc.)
	Instructions string // Voice style instructions (gpt-4o-mini-tts only)
	BaseURL      string // Custom API endpoint (default: "https://api.openai.com/v1")
	TimeoutSec   int
}

// OpenAISynthesizer uses the OpenAI TTS API.
type OpenAISynthesizer struct {
	config OpenAIConfig
	client *http.Client
}

// NewOpenAISynthesizer creates a new OpenAI TTS client.
func NewOpenAISynthesizer(cfg OpenAIConfig) *OpenAISynthesizer {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "tts-1"
	}
	if cfg.Voice == "" {
		cfg.Voice = "alloy"
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 60
	}
	return &OpenAISynthesizer{
		config: cfg,
		client: &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second},
	}
}

// speechRequest is the JSON body for POST /audio/speech.
// Voice can be a string (built-in) or {"id": "voice_xxx"} (custom).
type speechRequest struct {
	Model        string      `json:"model"`
	Input        string      `json:"input"`
	Voice        interface{} `json:"voice"`
	Instructions string      `json:"instructions,omitempty"`
}

type customVoiceRef struct {
	ID string `json:"id"`
}

// createVoiceResponse is the JSON response from POST /audio/voices.
type createVoiceResponse struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Name      string `json:"name"`
	Object    string `json:"object"`
}

// Synthesize generates audio using the default configured voice.
func (s *OpenAISynthesizer) Synthesize(text string) (string, error) {
	return s.synthesize(text, s.config.Voice)
}

// SynthesizeWithVoice generates audio using a specific voice (built-in name or custom voice ID).
func (s *OpenAISynthesizer) SynthesizeWithVoice(text, voiceID string) (string, error) {
	return s.synthesize(text, voiceID)
}

func (s *OpenAISynthesizer) synthesize(text string, voice interface{}) (string, error) {
	// If voice looks like a custom voice ID (e.g. "voice_xxx"), wrap it in {"id": "..."}
	var voiceField interface{}
	if v, ok := voice.(string); ok && strings.HasPrefix(v, "voice_") {
		voiceField = customVoiceRef{ID: v}
	} else {
		voiceField = voice
	}

	req := speechRequest{
		Model:        s.config.Model,
		Input:        text,
		Voice:        voiceField,
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

// CreateVoice uploads an audio sample to create a custom voice via POST /audio/voices.
// Returns the voice ID (e.g. "voice_xxx").
func (s *OpenAISynthesizer) CreateVoice(name, audioPath string) (string, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("open audio file: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("name", name); err != nil {
		return "", fmt.Errorf("write name field: %w", err)
	}
	// consent field is required by the API
	if err := writer.WriteField("consent", "consent"); err != nil {
		return "", fmt.Errorf("write consent field: %w", err)
	}

	part, err := writer.CreateFormFile("audio_sample", filepath.Base(audioPath))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("copy audio: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close writer: %w", err)
	}

	url := strings.TrimRight(s.config.BaseURL, "/") + "/audio/voices"
	httpReq, err := http.NewRequest("POST", url, &body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.config.APIKey)
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("create voice request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create voice returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result createVoiceResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.ID, nil
}

// saveResponse executes the request and saves audio response to a temp file.
func (s *OpenAISynthesizer) saveResponse(req *http.Request, ext string) (string, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tts API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
		return "", fmt.Errorf("tts API returned %d: %s", resp.StatusCode, string(body))
	}

	tmpDir := os.TempDir()
	localName := fmt.Sprintf("tts_%d.%s", time.Now().UnixMilli(), ext)
	localPath := filepath.Join(tmpDir, localName)

	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	written, err := io.Copy(f, io.LimitReader(resp.Body, 50*1024*1024+1))
	if err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("write audio: %w", err)
	}
	if written > 50*1024*1024 {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("audio too large (>50MB)")
	}

	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("close file: %w", err)
	}

	return localPath, nil
}

// Verify interface satisfaction.
var _ Synthesizer = (*OpenAISynthesizer)(nil)
