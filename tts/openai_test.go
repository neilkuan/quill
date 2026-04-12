package tts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestOpenAISynthesizer_Synthesize_Success(t *testing.T) {
	fakeAudio := []byte("fake-mp3-audio-data")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			t.Errorf("expected /audio/speech, got %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing Bearer token")
		}

		var req speechRequest
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		if req.Model != "tts-1" {
			t.Errorf("expected model 'tts-1', got %q", req.Model)
		}
		// Voice should be a string for built-in voices
		if req.Voice != "nova" {
			t.Errorf("expected voice 'nova', got %v", req.Voice)
		}

		w.Write(fakeAudio)
	}))
	defer server.Close()

	synth := NewOpenAISynthesizer(OpenAIConfig{
		APIKey:  "test-key",
		Model:   "tts-1",
		Voice:   "nova",
		BaseURL: server.URL,
	})

	audioPath, err := synth.Synthesize("Hello test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(audioPath)

	data, _ := os.ReadFile(audioPath)
	if string(data) != string(fakeAudio) {
		t.Errorf("unexpected audio content")
	}
}

func TestOpenAISynthesizer_SynthesizeWithVoice_CustomID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)

		// Custom voice ID should be wrapped as {"id": "voice_xxx"}
		voice, ok := req["voice"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected voice to be object, got %T: %v", req["voice"], req["voice"])
		}
		if voice["id"] != "voice_abc123" {
			t.Errorf("expected voice id 'voice_abc123', got %v", voice["id"])
		}

		w.Write([]byte("audio"))
	}))
	defer server.Close()

	synth := NewOpenAISynthesizer(OpenAIConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	audioPath, err := synth.SynthesizeWithVoice("test", "voice_abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	os.Remove(audioPath)
}

func TestOpenAISynthesizer_SynthesizeWithVoice_BuiltIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)

		// Built-in voice should be a plain string
		voice, ok := req["voice"].(string)
		if !ok {
			t.Fatalf("expected voice to be string, got %T: %v", req["voice"], req["voice"])
		}
		if voice != "shimmer" {
			t.Errorf("expected voice 'shimmer', got %q", voice)
		}

		w.Write([]byte("audio"))
	}))
	defer server.Close()

	synth := NewOpenAISynthesizer(OpenAIConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	audioPath, err := synth.SynthesizeWithVoice("test", "shimmer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	os.Remove(audioPath)
}

func TestOpenAISynthesizer_CreateVoice_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/voices" {
			t.Errorf("expected /audio/voices, got %s", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Error("expected multipart/form-data")
		}

		r.ParseMultipartForm(32 << 20)
		if r.FormValue("name") != "My Voice" {
			t.Errorf("expected name 'My Voice', got %q", r.FormValue("name"))
		}
		if r.FormValue("consent") != "consent" {
			t.Errorf("expected consent field")
		}
		_, _, err := r.FormFile("audio_sample")
		if err != nil {
			t.Fatalf("expected audio_sample file: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createVoiceResponse{
			ID:   "voice_test123",
			Name: "My Voice",
		})
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	audioPath := tmpDir + "/sample.wav"
	os.WriteFile(audioPath, []byte("fake-audio"), 0644)

	synth := NewOpenAISynthesizer(OpenAIConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	voiceID, err := synth.CreateVoice("My Voice", audioPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if voiceID != "voice_test123" {
		t.Errorf("expected 'voice_test123', got %q", voiceID)
	}
}

func TestOpenAISynthesizer_Synthesize_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API key"}}`))
	}))
	defer server.Close()

	synth := NewOpenAISynthesizer(OpenAIConfig{
		APIKey:  "bad-key",
		BaseURL: server.URL,
	})

	_, err := synth.Synthesize("test")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got %q", err.Error())
	}
}

func TestNewOpenAISynthesizer_Defaults(t *testing.T) {
	synth := NewOpenAISynthesizer(OpenAIConfig{APIKey: "test-key"})

	if synth.config.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("expected default base URL, got %q", synth.config.BaseURL)
	}
	if synth.config.Model != "tts-1" {
		t.Errorf("expected default model 'tts-1', got %q", synth.config.Model)
	}
	if synth.config.Voice != "alloy" {
		t.Errorf("expected default voice 'alloy', got %q", synth.config.Voice)
	}
}

var _ Synthesizer = (*OpenAISynthesizer)(nil)
