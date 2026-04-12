package tts

import (
	"testing"
)

func TestVoiceStore_SetGetRemove(t *testing.T) {
	store, err := NewVoiceStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewVoiceStore failed: %v", err)
	}

	if got := store.GetVoice("user1"); got != "" {
		t.Fatalf("expected empty voice, got %q", got)
	}

	if err := store.SetVoice("user1", "voice_abc123"); err != nil {
		t.Fatalf("SetVoice failed: %v", err)
	}

	if got := store.GetVoice("user1"); got != "voice_abc123" {
		t.Fatalf("expected 'voice_abc123', got %q", got)
	}

	if got := store.GetVoice("user2"); got != "" {
		t.Fatalf("expected empty for user2, got %q", got)
	}

	if err := store.RemoveVoice("user1"); err != nil {
		t.Fatalf("RemoveVoice failed: %v", err)
	}
	if got := store.GetVoice("user1"); got != "" {
		t.Fatalf("expected empty after remove, got %q", got)
	}
}

func TestVoiceStore_Persistence(t *testing.T) {
	dir := t.TempDir()

	store1, _ := NewVoiceStore(dir)
	store1.SetVoice("user1", "voice_xyz")

	// New store from same dir should load persisted data
	store2, _ := NewVoiceStore(dir)
	if got := store2.GetVoice("user1"); got != "voice_xyz" {
		t.Fatalf("expected persisted voice 'voice_xyz', got %q", got)
	}
}

func TestVoiceStore_EchoMode(t *testing.T) {
	store, _ := NewVoiceStore(t.TempDir())

	if store.IsEchoMode("user1") {
		t.Fatal("expected echo mode off by default")
	}

	store.SetEchoMode("user1", true)
	if !store.IsEchoMode("user1") {
		t.Fatal("expected echo mode on")
	}

	store.SetEchoMode("user1", false)
	if store.IsEchoMode("user1") {
		t.Fatal("expected echo mode off after disable")
	}
}
