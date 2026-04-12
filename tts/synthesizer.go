package tts

// Synthesizer converts text to an audio file.
type Synthesizer interface {
	// Synthesize generates an audio file from text and returns the local file path.
	// The caller is responsible for removing the file after use.
	Synthesize(text string) (audioPath string, err error)

	// SynthesizeWithVoice generates audio using a specific voice ID (custom or built-in).
	SynthesizeWithVoice(text, voiceID string) (audioPath string, err error)

	// CreateVoice uploads an audio sample to create a custom voice.
	// Returns the voice ID that can be used in SynthesizeWithVoice.
	CreateVoice(name, audioPath string) (voiceID string, err error)
}
