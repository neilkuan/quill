package command

import (
	"strings"
	"testing"
	"time"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/sessionpicker"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input   string
		wantCmd string
		wantOk  bool
	}{
		{"sessions", CmdSessions, true},
		{"Sessions", CmdSessions, true},
		{"SESSIONS", CmdSessions, true},
		{"reset", CmdReset, true},
		{"resume", CmdResume, true},
		{"Resume", CmdResume, true},
		{"RESUME", CmdResume, true},
		{"info", CmdInfo, true},
		{"stop", CmdStop, true},
		{"Stop", CmdStop, true},
		{"STOP", CmdStop, true},
		{"cancel", CmdStop, true}, // alias → stop
		{"CANCEL", CmdStop, true},
		{"pick", CmdPicker, true},
		{"Pick", CmdPicker, true},
		{"pick 3", CmdPicker, true},           // canonical with numeric arg
		{"history", CmdPicker, true},          // alias → pick
		{"history load abc", CmdPicker, true}, // alias with load subcommand
		{"mode", CmdMode, true},
		{"Mode", CmdMode, true},
		{"mode ask", CmdMode, true},
		{"mode 2", CmdMode, true},
		{"model", CmdModel, true},
		{"Model", CmdModel, true},
		{"model haiku", CmdModel, true},
		{"model 2", CmdModel, true},
		{"session-picker", CmdPicker, true},   // legacy alias for users typing the old form
		{"Session-Picker", CmdPicker, true},
		{"session_picker", CmdPicker, true},   // legacy alias (Telegram-friendly spelling)
		{"sessionpicker", CmdPicker, true},    // legacy alias (single-word spelling)
		{"sessions extra args", CmdSessions, true},
		{"hello world", "", false},
		{"", "", false},
		{"help", CmdHelp, true},
		{"Help", CmdHelp, true},
		{"HELP", CmdHelp, true},
		{"?", CmdHelp, true},        // alias → help
		{"commands", CmdHelp, true}, // alias → help
		{"   sessions   ", CmdSessions, true},
		{"reset now", CmdReset, true},
		{"session", "", false}, // not "sessions"
		// Telegram msg.Command() returns bare name without /
		// Discord slash commands also use bare names
		// Both pass through ParseCommand correctly
	}

	for _, tt := range tests {
		cmd, ok := ParseCommand(tt.input)
		if ok != tt.wantOk {
			t.Errorf("ParseCommand(%q): got ok=%v, want %v", tt.input, ok, tt.wantOk)
			continue
		}
		if ok && cmd.Name != tt.wantCmd {
			t.Errorf("ParseCommand(%q): got name=%q, want %q", tt.input, cmd.Name, tt.wantCmd)
		}
	}
}

func TestExecuteHelp_ListsEverySupportedCommand(t *testing.T) {
	out := ExecuteHelp()
	for _, name := range []string{
		CmdSessions, CmdInfo, CmdReset, CmdResume, CmdStop,
		CmdPicker, CmdMode, CmdModel, CmdCron, CmdHelp,
	} {
		if !strings.Contains(out, "`"+name+"`") {
			t.Errorf("ExecuteHelp output is missing command %q\n%s", name, out)
		}
	}
}

func TestParseCommand_Args(t *testing.T) {
	cmd, ok := ParseCommand("sessions foo bar")
	if !ok {
		t.Fatal("expected ok")
	}
	if cmd.Args != "foo bar" {
		t.Errorf("got args=%q, want %q", cmd.Args, "foo bar")
	}
}

// fakePicker is a stand-in sessionpicker.Picker for unit tests — no
// filesystem access, deterministic results.
type fakePicker struct {
	agentType string
	sessions  []sessionpicker.Session
	err       error
}

func (f *fakePicker) AgentType() string { return f.agentType }

func (f *fakePicker) List(cwd string, limit int) ([]sessionpicker.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := f.sessions
	if cwd != "" {
		filtered := out[:0:0]
		for _, s := range out {
			if s.CWD == cwd {
				filtered = append(filtered, s)
			}
		}
		out = filtered
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func TestExecutePicker_NilPickerGivesHelpfulMessage(t *testing.T) {
	msg := ExecutePicker(nil, nil, "discord:chan-1", "", "")
	if !strings.Contains(strings.ToLower(msg), "not supported") {
		t.Errorf("expected friendly 'not supported' message, got: %q", msg)
	}
}

func TestExecutePicker_ListsCachesAndLoadsByIndex(t *testing.T) {
	fake := &fakePicker{
		agentType: "kiro-cli",
		sessions: []sessionpicker.Session{
			{ID: "sess-1", Title: "first", CWD: "/work/a", UpdatedAt: time.Now().Add(-time.Hour)},
			{ID: "sess-2", Title: "second", CWD: "/work/a", UpdatedAt: time.Now().Add(-30 * time.Minute)},
		},
	}
	thread := "discord:chan-cache"

	out := ExecutePicker(nil, fake, thread, "", "/work/a")
	if !strings.Contains(out, "sess-1") || !strings.Contains(out, "sess-2") {
		t.Fatalf("listing should contain both session IDs (truncated): %s", out)
	}

	// After the listing call, the cache should remember the sessions
	// so an index lookup resolves.
	cached, ok := getPickerListing(thread)
	if !ok {
		t.Fatal("expected cached listing to be present after ExecutePicker")
	}
	if len(cached) != 2 {
		t.Errorf("cached length = %d, want 2", len(cached))
	}
}

func TestExecutePicker_OutOfRangeIndex(t *testing.T) {
	fake := &fakePicker{agentType: "kiro-cli"}
	thread := "discord:chan-oor"
	// Prime cache with one entry.
	cachePickerListing(thread, []sessionpicker.Session{
		{ID: "only", Title: "t", UpdatedAt: time.Now()},
	})

	msg := ExecutePicker(nil, fake, thread, "5", "")
	if !strings.Contains(strings.ToLower(msg), "out of range") {
		t.Errorf("expected out-of-range message, got: %q", msg)
	}
}

func TestExecutePicker_NoCacheForIndex(t *testing.T) {
	fake := &fakePicker{agentType: "kiro-cli"}
	// Unique thread key so no prior cache applies.
	msg := ExecutePicker(nil, fake, "thread-no-cache", "2", "")
	if !strings.Contains(strings.ToLower(msg), "no recent listing") {
		t.Errorf("expected 'no recent listing' when cache is empty, got: %q", msg)
	}
}

func TestExecutePicker_UnknownSubcommand(t *testing.T) {
	fake := &fakePicker{agentType: "kiro-cli"}
	msg := ExecutePicker(nil, fake, "discord:chan-usage", "???", "")
	if !strings.Contains(strings.ToLower(msg), "usage") {
		t.Errorf("expected usage hint, got: %q", msg)
	}
}

func TestExecutePicker_EmptyListSuggestsAll(t *testing.T) {
	// Picker has sessions, but none match the cwdFilter — output
	// should hint that `all` exists.
	fake := &fakePicker{
		agentType: "codex-acp",
		sessions:  []sessionpicker.Session{{ID: "x", Title: "y", CWD: "/elsewhere", UpdatedAt: time.Now()}},
	}
	msg := ExecutePicker(nil, fake, "discord:chan-empty", "", "/no/match")
	if !strings.Contains(msg, "/pick all") {
		t.Errorf("expected hint about `/pick all`, got: %q", msg)
	}
}

func TestListPickerSessions_NilPickerReturnsErrListing(t *testing.T) {
	listing := ListPickerSessions(nil, "discord:chan-x", "/any", false)
	if listing.Err == nil {
		t.Fatal("expected Err to be set when picker is nil")
	}
	if !strings.Contains(strings.ToLower(listing.Message), "not supported") {
		t.Errorf("expected friendly message, got: %q", listing.Message)
	}
	if len(listing.Sessions) != 0 {
		t.Errorf("Sessions should be empty, got %d", len(listing.Sessions))
	}
}

func TestListPickerSessions_PopulatesSessionsAndCache(t *testing.T) {
	fake := &fakePicker{
		agentType: "kiro-cli",
		sessions: []sessionpicker.Session{
			{ID: "a", Title: "one", CWD: "/work", UpdatedAt: time.Now().Add(-time.Hour)},
			{ID: "b", Title: "two", CWD: "/work", UpdatedAt: time.Now().Add(-30 * time.Minute)},
		},
	}
	thread := "discord:chan-pick-list"

	listing := ListPickerSessions(fake, thread, "/work", false)
	if listing.Err != nil {
		t.Fatalf("unexpected error: %v", listing.Err)
	}
	if listing.AgentType != "kiro-cli" {
		t.Errorf("AgentType = %q", listing.AgentType)
	}
	if listing.CWD != "/work" || listing.BypassCWD {
		t.Errorf("cwd metadata wrong: cwd=%q bypass=%v", listing.CWD, listing.BypassCWD)
	}
	if len(listing.Sessions) != 2 {
		t.Fatalf("Sessions len = %d, want 2", len(listing.Sessions))
	}

	// Cache should now resolve to the same sessions — the
	// callback-data path (and `/pick <N>`) depends on this.
	cached, ok := getPickerListing(thread)
	if !ok || len(cached) != 2 {
		t.Errorf("expected cache to hold 2 entries, got ok=%v len=%d", ok, len(cached))
	}
}

func TestListPickerSessions_BypassCWD(t *testing.T) {
	fake := &fakePicker{
		agentType: "codex-acp",
		sessions: []sessionpicker.Session{
			{ID: "x", Title: "t", CWD: "/elsewhere", UpdatedAt: time.Now()},
		},
	}
	// When bypassCWD is true, the listing uses the unfiltered path
	// (cwd dropped) — verifies all-sessions mode reaches the picker.
	listing := ListPickerSessions(fake, "discord:chan-bypass", "/not-a-match", true)
	if listing.Err != nil {
		t.Fatalf("unexpected error: %v", listing.Err)
	}
	if !listing.BypassCWD {
		t.Error("BypassCWD should be true")
	}
	if len(listing.Sessions) != 1 {
		t.Fatalf("expected 1 session from all-cwds listing, got %d", len(listing.Sessions))
	}
}

func TestLoadPickerByIndex_ResolvesFromCache(t *testing.T) {
	thread := "discord:chan-load-idx"
	cachePickerListing(thread, []sessionpicker.Session{
		{ID: "sess-1", CWD: "/w"}, {ID: "sess-2", CWD: "/w"},
	})

	// With no pool the load itself will fail, but we only care that
	// the index-resolution path reaches the pool layer (i.e. doesn't
	// short-circuit at "no cache") — so we exercise an out-of-range
	// case for determinism.
	msg := LoadPickerByIndex(nil, thread, 99)
	if !strings.Contains(strings.ToLower(msg), "out of range") {
		t.Errorf("expected out-of-range, got: %q", msg)
	}
}

func TestPickerCache_TTLExpiry(t *testing.T) {
	thread := "thread-ttl"
	cachePickerListing(thread, []sessionpicker.Session{{ID: "a"}})
	// Manually age the entry past the TTL to avoid a real sleep.
	pickerCacheMu.Lock()
	e := pickerCache[thread]
	e.expiresAt = time.Now().Add(-time.Second)
	pickerCache[thread] = e
	pickerCacheMu.Unlock()

	if _, ok := getPickerListing(thread); ok {
		t.Error("expected expired cache entry to be absent")
	}
}

func TestFormatModeListing(t *testing.T) {
	modes := []acp.ModeInfo{
		{ID: "ask", Name: "Ask", Description: "Question only"},
		{ID: "code", Name: "Code"},  // description optional
		{ID: "bare"},                // name optional — id shown once
		{ID: "agent-x", Name: "agent-x"}, // name duplicates id (Kiro shape) — name suppressed
	}
	out := formatModeListing("code", modes)

	// Current mode marker, index, name/description all rendered.
	if !strings.Contains(out, "➤") {
		t.Error("current mode should be marked with an arrow")
	}
	if !strings.Contains(out, "`2.` `code`") {
		t.Errorf("expected index marker for code entry: %s", out)
	}
	if !strings.Contains(out, "Question only") {
		t.Errorf("description missing: %s", out)
	}
	if !strings.Contains(out, "`bare`") {
		t.Errorf("id row missing for bare entry: %s", out)
	}
	// For the Kiro-shape row (id == name), the id should appear once
	// and the name should not be duplicated after an em dash.
	if strings.Contains(out, "`agent-x` — agent-x") {
		t.Errorf("name duplicating id should be suppressed: %s", out)
	}
	if !strings.Contains(out, "`agent-x`") {
		t.Errorf("expected id for agent-x entry: %s", out)
	}
}

func TestIsKnownMode(t *testing.T) {
	modes := []acp.ModeInfo{{ID: "ask"}, {ID: "code"}}
	if !isKnownMode(modes, "ask") {
		t.Error("ask should be known")
	}
	if isKnownMode(modes, "nope") {
		t.Error("nope should not be known")
	}
	if isKnownMode(nil, "ask") {
		t.Error("empty list: nothing is known")
	}
}

func TestJoinModeIDs(t *testing.T) {
	got := joinModeIDs([]acp.ModeInfo{{ID: "ask"}, {ID: "code"}})
	if got != "`ask`, `code`" {
		t.Errorf("joinModeIDs = %q", got)
	}
}

func TestFormatModelListing(t *testing.T) {
	models := []acp.ModelInfo{
		{ID: "haiku", Name: "Haiku 4.5", Description: "Fast"},
		{ID: "sonnet", Name: "Sonnet 4.6"},
		{ID: "bare"},
		{ID: "kiro-default", Name: "kiro-default"}, // name == id: suppressed
	}
	out := formatModelListing("sonnet", models)

	if !strings.Contains(out, "➤") {
		t.Error("current model should be marked")
	}
	if !strings.Contains(out, "`2.` `sonnet`") {
		t.Errorf("expected index marker: %s", out)
	}
	if !strings.Contains(out, "Fast") {
		t.Errorf("description missing: %s", out)
	}
	if strings.Contains(out, "`kiro-default` — kiro-default") {
		t.Errorf("name duplicating id must be suppressed: %s", out)
	}
}

func TestIsKnownModel(t *testing.T) {
	models := []acp.ModelInfo{{ID: "haiku"}, {ID: "sonnet"}}
	if !isKnownModel(models, "haiku") {
		t.Error("haiku should be known")
	}
	if isKnownModel(models, "nope") {
		t.Error("nope should not be known")
	}
}

func TestJoinModelIDs(t *testing.T) {
	got := joinModelIDs([]acp.ModelInfo{{ID: "haiku"}, {ID: "sonnet"}})
	if got != "`haiku`, `sonnet`" {
		t.Errorf("joinModelIDs = %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestParseCommandCronAdd(t *testing.T) {
	cmd, ok := ParseCommand("cron add 0 9 * * * daily standup")
	if !ok || cmd.Name != CmdCron {
		t.Fatalf("ParseCommand returned ok=%v cmd=%+v", ok, cmd)
	}
	if cmd.Args == "" {
		t.Error("expected args to be populated")
	}
}

func TestParseCronAddArgs(t *testing.T) {
	args := "add 0 9 * * * daily standup"
	sub, schedule, prompt, err := ParseCronArgs(args)
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if sub != "add" {
		t.Errorf("sub=%q", sub)
	}
	if schedule != "0 9 * * *" {
		t.Errorf("schedule=%q", schedule)
	}
	if prompt != "daily standup" {
		t.Errorf("prompt=%q", prompt)
	}
}

func TestParseCronAddArgsEvery(t *testing.T) {
	_, schedule, prompt, err := ParseCronArgs("add every 5m ping the build")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if schedule != "every 5m" {
		t.Errorf("schedule=%q want 'every 5m'", schedule)
	}
	if prompt != "ping the build" {
		t.Errorf("prompt=%q", prompt)
	}
}

func TestParseCronAddArgsAtAbsolute(t *testing.T) {
	_, schedule, prompt, err := ParseCronArgs("add at 2026-05-05 09:00 launch deploy")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if schedule != "at 2026-05-05 09:00" {
		t.Errorf("schedule=%q", schedule)
	}
	if prompt != "launch deploy" {
		t.Errorf("prompt=%q", prompt)
	}
}

func TestParseCronListEmpty(t *testing.T) {
	sub, schedule, prompt, err := ParseCronArgs("list")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if sub != "list" || schedule != "" || prompt != "" {
		t.Errorf("got sub=%q schedule=%q prompt=%q", sub, schedule, prompt)
	}
}

func TestParseCronRm(t *testing.T) {
	sub, schedule, _, err := ParseCronArgs("rm abc12345")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if sub != "rm" {
		t.Errorf("sub=%q", sub)
	}
	if schedule != "abc12345" {
		t.Errorf("expected id in schedule slot, got %q", schedule)
	}
}
