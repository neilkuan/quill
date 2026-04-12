package tts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// VoiceStore manages per-user custom voice IDs.
// Voice IDs (from OpenAI Create Voice API) are stored in a JSON file on disk.
type VoiceStore struct {
	path string
	mu   sync.RWMutex
	data voiceData
}

type voiceData struct {
	Voices   map[string]string `json:"voices"`    // userID → voice_id
	EchoMode map[string]bool   `json:"echo_mode"` // userID → enabled
}

// NewVoiceStore creates a voice store at {baseDir}/.voices.json
func NewVoiceStore(baseDir string) (*VoiceStore, error) {
	path := filepath.Join(baseDir, ".voices.json")
	vs := &VoiceStore{
		path: path,
		data: voiceData{
			Voices:   make(map[string]string),
			EchoMode: make(map[string]bool),
		},
	}
	// Load existing data if file exists
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &vs.data)
		if vs.data.Voices == nil {
			vs.data.Voices = make(map[string]string)
		}
		if vs.data.EchoMode == nil {
			vs.data.EchoMode = make(map[string]bool)
		}
	}
	return vs, nil
}

// SetVoice stores a custom voice ID for a user.
func (vs *VoiceStore) SetVoice(userID, voiceID string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.data.Voices[userID] = voiceID
	return vs.save()
}

// GetVoice returns the custom voice ID for a user, or empty string if not set.
func (vs *VoiceStore) GetVoice(userID string) string {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.data.Voices[userID]
}

// RemoveVoice deletes a user's custom voice.
func (vs *VoiceStore) RemoveVoice(userID string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	delete(vs.data.Voices, userID)
	delete(vs.data.EchoMode, userID)
	return vs.save()
}

// SetEchoMode enables or disables echo mode for a user.
func (vs *VoiceStore) SetEchoMode(userID string, enabled bool) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if enabled {
		vs.data.EchoMode[userID] = true
	} else {
		delete(vs.data.EchoMode, userID)
	}
	vs.save()
}

// IsEchoMode returns whether echo mode is active for a user.
func (vs *VoiceStore) IsEchoMode(userID string) bool {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.data.EchoMode[userID]
}

func (vs *VoiceStore) save() error {
	raw, err := json.MarshalIndent(vs.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal voices: %w", err)
	}
	return os.WriteFile(vs.path, raw, 0600)
}
