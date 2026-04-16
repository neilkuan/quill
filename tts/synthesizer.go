package tts

// Synthesizer converts text to an audio file.
type Synthesizer interface {
	// Synthesize generates an audio file from text and returns the local file path.
	// The caller is responsible for removing the file after use.
	Synthesize(text string) (audioPath string, err error)
}
