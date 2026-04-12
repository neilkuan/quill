package stt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAITranscriber_Transcribe_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request method and auth header
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing Bearer token in Authorization header")
		}
		if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Error("expected multipart/form-data content type")
		}

		// Parse multipart form
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("failed to parse multipart form: %v", err)
		}

		// Verify expected fields
		if r.FormValue("model") != "whisper-1" {
			t.Errorf("expected model 'whisper-1', got %q", r.FormValue("model"))
		}
		if r.FormValue("language") != "zh" {
			t.Errorf("expected language 'zh', got %q", r.FormValue("language"))
		}
		if r.FormValue("prompt") == "" {
			t.Error("expected non-empty prompt field")
		}

		// Verify file upload
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("failed to get form file: %v", err)
		}
		defer file.Close()

		if header.Filename != "test.ogg" {
			t.Errorf("expected filename 'test.ogg', got %q", header.Filename)
		}

		data, _ := io.ReadAll(file)
		if string(data) != "fake-audio-data" {
			t.Errorf("unexpected file content: %q", string(data))
		}

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(whisperResponse{Text: "你好，這是一段測試語音"})
	}))
	defer server.Close()

	// Create a fake audio file
	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "test.ogg")
	if err := os.WriteFile(audioPath, []byte("fake-audio-data"), 0644); err != nil {
		t.Fatalf("failed to write test audio file: %v", err)
	}

	transcriber := NewOpenAITranscriber(OpenAIConfig{
		APIKey:   "test-api-key",
		Model:    "whisper-1",
		Language: "zh",
		Prompt:   "以下是繁體中文語音的逐字稿：",
		BaseURL:  server.URL,
	})

	text, err := transcriber.Transcribe(audioPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "你好，這是一段測試語音" {
		t.Errorf("unexpected transcription: %q", text)
	}
}

func TestOpenAITranscriber_Transcribe_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API key"}}`))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "test.ogg")
	os.WriteFile(audioPath, []byte("fake-audio"), 0644)

	transcriber := NewOpenAITranscriber(OpenAIConfig{
		APIKey:  "bad-key",
		BaseURL: server.URL,
	})

	_, err := transcriber.Transcribe(audioPath)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected error to mention 401, got %q", err.Error())
	}
}

func TestOpenAITranscriber_Transcribe_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "test.ogg")
	os.WriteFile(audioPath, []byte("fake-audio"), 0644)

	transcriber := NewOpenAITranscriber(OpenAIConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	_, err := transcriber.Transcribe(audioPath)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention 500, got %q", err.Error())
	}
}

func TestOpenAITranscriber_Transcribe_FileNotFound(t *testing.T) {
	transcriber := NewOpenAITranscriber(OpenAIConfig{
		APIKey: "test-key",
	})

	_, err := transcriber.Transcribe("/nonexistent/path/audio.ogg")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "open audio file") {
		t.Errorf("expected 'open audio file' in error, got %q", err.Error())
	}
}

func TestOpenAITranscriber_Transcribe_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not valid json"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "test.ogg")
	os.WriteFile(audioPath, []byte("fake-audio"), 0644)

	transcriber := NewOpenAITranscriber(OpenAIConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	_, err := transcriber.Transcribe(audioPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected 'decode response' in error, got %q", err.Error())
	}
}

func TestOpenAITranscriber_Transcribe_ConnectionError(t *testing.T) {
	transcriber := NewOpenAITranscriber(OpenAIConfig{
		APIKey:  "test-key",
		BaseURL: "http://127.0.0.1:1",
	})

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "test.ogg")
	os.WriteFile(audioPath, []byte("fake-audio"), 0644)

	_, err := transcriber.Transcribe(audioPath)
	if err == nil {
		t.Fatal("expected error for connection failure")
	}
	if !strings.Contains(err.Error(), "whisper API request failed") {
		t.Errorf("expected 'whisper API request failed' in error, got %q", err.Error())
	}
}

func TestOpenAITranscriber_Transcribe_OptionalFields(t *testing.T) {
	var receivedLanguage, receivedPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(32 << 20)
		receivedLanguage = r.FormValue("language")
		receivedPrompt = r.FormValue("prompt")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(whisperResponse{Text: "hello"})
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "test.ogg")
	os.WriteFile(audioPath, []byte("fake-audio"), 0644)

	// No language or prompt set
	transcriber := NewOpenAITranscriber(OpenAIConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	text, err := transcriber.Transcribe(audioPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hello" {
		t.Errorf("unexpected text: %q", text)
	}
	if receivedLanguage != "" {
		t.Errorf("expected empty language, got %q", receivedLanguage)
	}
	if receivedPrompt != "" {
		t.Errorf("expected empty prompt, got %q", receivedPrompt)
	}
}

func TestNewOpenAITranscriber_Defaults(t *testing.T) {
	tr := NewOpenAITranscriber(OpenAIConfig{
		APIKey: "test-key",
	})

	if tr.config.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("expected default base URL, got %q", tr.config.BaseURL)
	}
	if tr.config.Model != "whisper-1" {
		t.Errorf("expected default model 'whisper-1', got %q", tr.config.Model)
	}
}

func TestNewOpenAITranscriber_CustomValues(t *testing.T) {
	tr := NewOpenAITranscriber(OpenAIConfig{
		APIKey:  "my-key",
		Model:   "whisper-large-v3",
		BaseURL: "https://custom.api.com/v1",
	})

	if tr.config.BaseURL != "https://custom.api.com/v1" {
		t.Errorf("expected custom base URL, got %q", tr.config.BaseURL)
	}
	if tr.config.Model != "whisper-large-v3" {
		t.Errorf("expected custom model, got %q", tr.config.Model)
	}
}

// Verify Transcriber interface is satisfied
var _ Transcriber = (*OpenAITranscriber)(nil)
