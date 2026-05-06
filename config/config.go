package config

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"

	"github.com/BurntSushi/toml"
)

// --- Top-level ---

type Config struct {
	Agent      AgentConfig      `toml:"agent"`
	Pool       PoolConfig       `toml:"pool"`
	API        APIConfig        `toml:"api"`
	STT     STTConfig     `toml:"stt"`
	TTS     TTSConfig     `toml:"tts"`
	Markdown   MarkdownConfig   `toml:"markdown"`
	Discord DiscordConfig `toml:"discord"`
	Telegram   TelegramConfig   `toml:"telegram"`
	Teams      TeamsConfig      `toml:"teams"`
	Cronjob    CronjobConfig    `toml:"cronjob"`
}

// --- Markdown ---

// MarkdownConfig controls the channel-agnostic markdown rewrite pipeline applied
// to LLM responses before they are sent to a chat platform.
//
// `tables` accepts "code" (wrap in fenced block, default), "bullets" (one bullet
// per cell), or "off" (disable conversion).
type MarkdownConfig struct {
	Tables string `toml:"tables"`
}

// --- Cronjob ---

// CronjobConfig controls user-scheduled prompt fires. The zero value
// (no [cronjob] block in TOML) means "enabled with defaults". Users
// opt out with `disabled = true`.
type CronjobConfig struct {
	Disabled           bool   `toml:"disabled"`
	MaxPerThread       int    `toml:"max_per_thread"`
	MinIntervalSeconds int    `toml:"min_interval_seconds"`
	QueueSize          int    `toml:"queue_size"`
	Timezone           string `toml:"timezone"`
	StorePath          string `toml:"store_path"`
}

// --- Shared ---

type AgentConfig struct {
	Command    string            `toml:"command"`
	Args       []string          `toml:"args"`
	WorkingDir string            `toml:"working_dir"`
	Env        map[string]string `toml:"env"`
}

type PoolConfig struct {
	MaxSessions     int `toml:"max_sessions"`
	SessionTTLHours int `toml:"session_ttl_hours"`
}

// --- Discord ---

type DiscordConfig struct {
	Enabled         bool            `toml:"enabled"`
	BotToken        string          `toml:"bot_token"`
	AllowedChannels []string        `toml:"allowed_channels"`
	// AllowedUserIDs, when non-empty, takes precedence over AllowedChannels:
	// only messages from these Discord user IDs will be processed (regardless of channel).
	// Use ["*"] as a wildcard to allow any user from any channel.
	AllowedUserIDs []string        `toml:"allowed_user_id"`
	Reactions      ReactionsConfig `toml:"reactions"`
}

type ReactionsConfig struct {
	Enabled          bool           `toml:"enabled"`
	RemoveAfterReply bool           `toml:"remove_after_reply"`
	// ToolDisplay controls how ACP tool-call titles are rendered in the
	// streamed chat message. One of:
	//   "full"    — original title (e.g. `Running: curl -s "https://..."`)
	//   "compact" — first whitespace-delimited token only (default)
	//   "none"    — hide tool lines entirely
	ToolDisplay string         `toml:"tool_display"`
	Emojis      ReactionEmojis `toml:"emojis"`
	Timing      ReactionTiming `toml:"timing"`
}

type ReactionEmojis struct {
	Queued   string `toml:"queued"`
	Thinking string `toml:"thinking"`
	Tool     string `toml:"tool"`
	Coding   string `toml:"coding"`
	Web      string `toml:"web"`
	Done     string `toml:"done"`
	Error    string `toml:"error"`
}

type ReactionTiming struct {
	DebounceMs  int64 `toml:"debounce_ms"`
	StallSoftMs int64 `toml:"stall_soft_ms"`
	StallHardMs int64 `toml:"stall_hard_ms"`
	DoneHoldMs  int64 `toml:"done_hold_ms"`
	ErrorHoldMs int64 `toml:"error_hold_ms"`
}

// --- API ---

type APIConfig struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
}

// --- STT (Speech-to-Text) ---

type STTConfig struct {
	Enabled  bool   `toml:"enabled"`
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
	Model    string `toml:"model"`
	Language string `toml:"language"`
	Prompt   string `toml:"prompt"`
	BaseURL  string `toml:"base_url"`
}

// --- TTS (Text-to-Speech) ---

type TTSConfig struct {
	Enabled      bool   `toml:"enabled"`
	Provider     string `toml:"provider"`      // "openai" or "gemini" (default: "openai")
	APIKey       string `toml:"api_key"`
	Model        string `toml:"model"`
	Voice        string `toml:"voice"`
	Instructions string `toml:"instructions"`  // Voice style/tone instructions
	Style        string `toml:"style"`         // Gemini only: vocal_smile, newscaster, whisper, empathetic, promo_hype, deadpan
	StylePrefix  string `toml:"style_prefix"`  // Gemini only: emotion tag prepended to each line, e.g. "[shy]"
	StyleSuffix  string `toml:"style_suffix"`  // Gemini only: emotion tag appended to each line, e.g. "[laughs softly]"
	VoiceGender  string `toml:"voice_gender"`  // OpenAI only; ignored when provider != "openai"
	BaseURL      string `toml:"base_url"`      // OpenAI only
	TimeoutSec   int    `toml:"timeout_sec"`
}

// --- Telegram ---

type TelegramConfig struct {
	Enabled      bool    `toml:"enabled"`
	BotToken     string  `toml:"bot_token"`
	AllowedChats []int64 `toml:"allowed_chats"`
	// AllowedUserIDs, when non-empty, takes precedence over AllowedChats:
	// only messages from these Telegram user IDs will be processed (regardless of chat).
	// Use ["*"] as a wildcard to allow any user from any chat.
	// String type so "*" and numeric IDs can coexist in a single TOML array.
	AllowedUserIDs []string        `toml:"allowed_user_id"`
	Reactions      ReactionsConfig `toml:"reactions"`
}

// --- Teams ---

type TeamsConfig struct {
	Enabled        bool     `toml:"enabled"`
	AppID          string   `toml:"app_id"`
	AppSecret      string   `toml:"app_secret"`
	TenantID       string   `toml:"tenant_id"`
	Listen         string   `toml:"listen"`
	AllowedChannels []string `toml:"allowed_channels"`
	// AllowedUserIDs, when non-empty, takes precedence over AllowedChannels:
	// only messages from these Teams user IDs will be processed (regardless of channel).
	// Use ["*"] as a wildcard to allow any user.
	AllowedUserIDs []string `toml:"allowed_user_id"`
	ToolDisplay    string   `toml:"tool_display"`
	// ServiceURLStorePath persists the per-conversation Bot Framework
	// serviceURL so cron-fired proactive messages survive pod restarts.
	// Empty disables persistence (in-memory only — old behaviour).
	ServiceURLStorePath string `toml:"service_url_store_path"`
}

// --- Defaults ---

func applyDefaults(cfg *Config) {
	// Agent
	if cfg.Agent.WorkingDir == "" {
		cfg.Agent.WorkingDir = "/tmp"
	}

	// Pool
	if cfg.Pool.MaxSessions == 0 {
		cfg.Pool.MaxSessions = 10
	}
	if cfg.Pool.SessionTTLHours == 0 {
		cfg.Pool.SessionTTLHours = 24
	}

	// STT (Speech-to-Text)
	applySTTDefaults(&cfg.STT)

	// TTS (Text-to-Speech)
	applyTTSDefaults(&cfg.TTS)

	// Markdown pipeline
	if cfg.Markdown.Tables == "" {
		cfg.Markdown.Tables = "code"
	}

	// Discord — if the section is present with a token, default to enabled
	if cfg.Discord.BotToken != "" && !cfg.Discord.Enabled {
		cfg.Discord.Enabled = true
	}

	applyReactionDefaults(&cfg.Discord.Reactions)

	// Telegram — if the section is present with a token, default to enabled
	if cfg.Telegram.BotToken != "" && !cfg.Telegram.Enabled {
		cfg.Telegram.Enabled = true
	}

	applyTelegramReactionDefaults(&cfg.Telegram.Reactions)

	// Teams — if the section has app_id and app_secret, default to enabled
	if cfg.Teams.AppID != "" && cfg.Teams.AppSecret != "" && !cfg.Teams.Enabled {
		cfg.Teams.Enabled = true
	}
	if cfg.Teams.Listen == "" {
		cfg.Teams.Listen = ":3978"
	}
	if cfg.Teams.ServiceURLStorePath == "" {
		cfg.Teams.ServiceURLStorePath = "./.quill/teams-serviceurls.json"
	}

	// Cronjob — defaults if [cronjob] block omitted or partial. Disabled
	// is intentionally left untouched: zero value means "enabled".
	if cfg.Cronjob.MaxPerThread == 0 {
		cfg.Cronjob.MaxPerThread = 20
	}
	if cfg.Cronjob.MinIntervalSeconds == 0 {
		cfg.Cronjob.MinIntervalSeconds = 60
	}
	if cfg.Cronjob.QueueSize == 0 {
		cfg.Cronjob.QueueSize = 50
	}
	if cfg.Cronjob.Timezone == "" {
		cfg.Cronjob.Timezone = "UTC"
	}
	if cfg.Cronjob.StorePath == "" {
		cfg.Cronjob.StorePath = "./.quill/cronjobs.json"
	}
}

func applySTTDefaults(tc *STTConfig) {
	if tc.APIKey != "" && !tc.Enabled {
		tc.Enabled = true
	}
	if tc.Provider == "" {
		tc.Provider = "openai"
	}
	if tc.Model == "" {
		tc.Model = "whisper-1"
	}
	if tc.Language == "" {
		tc.Language = "zh"
	}
	if tc.Prompt == "" {
		tc.Prompt = "以下是繁體中文語音的逐字稿："
	}
}

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

func applyReactionDefaults(r *ReactionsConfig) {
	if !r.Enabled && r.Emojis.Queued == "" {
		r.Enabled = true
	}

	if r.ToolDisplay == "" {
		r.ToolDisplay = "compact"
	}

	e := &r.Emojis
	if e.Queued == "" {
		e.Queued = "👀"
	}
	if e.Thinking == "" {
		e.Thinking = "🤔"
	}
	if e.Tool == "" {
		e.Tool = "🔥"
	}
	if e.Coding == "" {
		e.Coding = "👨‍💻"
	}
	if e.Web == "" {
		e.Web = "⚡"
	}
	if e.Done == "" {
		e.Done = "🆗"
	}
	if e.Error == "" {
		e.Error = "😱"
	}

	t := &r.Timing
	if t.DebounceMs == 0 {
		t.DebounceMs = 700
	}
	if t.StallSoftMs == 0 {
		t.StallSoftMs = 10_000
	}
	if t.StallHardMs == 0 {
		t.StallHardMs = 30_000
	}
	if t.DoneHoldMs == 0 {
		t.DoneHoldMs = 1_500
	}
	if t.ErrorHoldMs == 0 {
		t.ErrorHoldMs = 2_500
	}
}

// applyTelegramReactionDefaults uses Telegram's standard reaction emoji set.
// See https://core.telegram.org/bots/api#reactiontypeemoji
func applyTelegramReactionDefaults(r *ReactionsConfig) {
	if !r.Enabled && r.Emojis.Queued == "" {
		r.Enabled = true
	}

	if r.ToolDisplay == "" {
		r.ToolDisplay = "compact"
	}

	e := &r.Emojis
	if e.Queued == "" {
		e.Queued = "👌"
	}
	if e.Thinking == "" {
		e.Thinking = "🤔"
	}
	if e.Tool == "" {
		e.Tool = "🔥"
	}
	if e.Coding == "" {
		e.Coding = "🤓"
	}
	if e.Web == "" {
		e.Web = "⚡"
	}
	if e.Done == "" {
		e.Done = "👍"
	}
	if e.Error == "" {
		e.Error = "😱"
	}

	t := &r.Timing
	if t.DebounceMs == 0 {
		t.DebounceMs = 700
	}
	if t.StallSoftMs == 0 {
		t.StallSoftMs = 10_000
	}
	if t.StallHardMs == 0 {
		t.StallHardMs = 30_000
	}
	if t.DoneHoldMs == 0 {
		t.DoneHoldMs = 1_500
	}
	if t.ErrorHoldMs == 0 {
		t.ErrorHoldMs = 2_500
	}
}

// --- Loader ---

var envVarRe = regexp.MustCompile(`\$\{(\w+)\}`)

func expandEnvVars(raw string) string {
	return envVarRe.ReplaceAllStringFunc(raw, func(match string) string {
		key := envVarRe.FindStringSubmatch(match)[1]
		return os.Getenv(key)
	})
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	expanded := expandEnvVars(string(raw))

	var cfg Config
	if err := toml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	if cfg.Agent.Command == "" {
		return fmt.Errorf("agent.command is required")
	}
	if _, err := exec.LookPath(cfg.Agent.Command); err != nil {
		return fmt.Errorf("agent.command %q not found in PATH: %w", cfg.Agent.Command, err)
	}
	if cfg.Agent.WorkingDir != "" {
		info, err := os.Stat(cfg.Agent.WorkingDir)
		if err != nil {
			return fmt.Errorf("agent.working_dir %q does not exist: %w", cfg.Agent.WorkingDir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("agent.working_dir %q is not a directory", cfg.Agent.WorkingDir)
		}
	}
	if !cfg.Discord.Enabled && !cfg.Telegram.Enabled && !cfg.Teams.Enabled {
		return fmt.Errorf("no platform enabled — set at least one of discord/telegram/teams")
	}
	return nil
}
