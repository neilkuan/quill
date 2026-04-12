package stt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Transcriber converts an audio file to text.
type Transcriber interface {
	Transcribe(audioPath string) (string, error)
}

// OpenAIConfig holds configuration for the OpenAI Whisper API.
type OpenAIConfig struct {
	APIKey   string
	Model    string
	Language string
	Prompt   string
	BaseURL  string
}

// OpenAITranscriber uses the OpenAI Whisper API for speech-to-text.
type OpenAITranscriber struct {
	config OpenAIConfig
	client *http.Client
}

// NewOpenAITranscriber creates a new OpenAI Whisper transcriber.
func NewOpenAITranscriber(cfg OpenAIConfig) *OpenAITranscriber {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "whisper-1"
	}
	return &OpenAITranscriber{
		config: cfg,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// whisperResponse is the JSON response from the Whisper API.
type whisperResponse struct {
	Text string `json:"text"`
}

// Transcribe sends the audio file to the Whisper API and returns the transcribed text.
func (t *OpenAITranscriber) Transcribe(audioPath string) (string, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("open audio file: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("copy audio data: %w", err)
	}

	if err := writer.WriteField("model", t.config.Model); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}
	if t.config.Language != "" {
		if err := writer.WriteField("language", t.config.Language); err != nil {
			return "", fmt.Errorf("write language field: %w", err)
		}
	}
	if t.config.Prompt != "" {
		if err := writer.WriteField("prompt", t.config.Prompt); err != nil {
			return "", fmt.Errorf("write prompt field: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	url := t.config.BaseURL + "/audio/transcriptions"
	req, err := http.NewRequest("POST", url, &body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.config.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result whisperResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.Text, nil
}
