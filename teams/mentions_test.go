package teams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMentionDirectory_RecordAndResolve(t *testing.T) {
	d := NewMentionDirectory()
	d.Record(Account{ID: "29:abc", Name: "Neil Kuan", AADObjectID: "guid-1"})

	got, ok := d.Resolve("Neil Kuan")
	if !ok {
		t.Fatalf("Resolve(\"Neil Kuan\") = false, want true")
	}
	if got.ID != "29:abc" {
		t.Errorf("Resolve.ID = %q, want %q", got.ID, "29:abc")
	}
	if got.Name != "Neil Kuan" {
		t.Errorf("Resolve.Name = %q, want %q", got.Name, "Neil Kuan")
	}
	if got.AADObjectID != "guid-1" {
		t.Errorf("Resolve.AADObjectID = %q, want %q", got.AADObjectID, "guid-1")
	}

	// Case + whitespace insensitive.
	if _, ok := d.Resolve("  neil kuan  "); !ok {
		t.Errorf("Resolve(\"  neil kuan  \") = false, want true")
	}
}

func TestMentionDirectory_RecordSkipsIncomplete(t *testing.T) {
	d := NewMentionDirectory()
	d.Record(Account{ID: "", Name: "No ID"})
	d.Record(Account{ID: "29:xyz", Name: ""})
	d.Record(Account{ID: "29:xyz", Name: "  "})

	if _, ok := d.Resolve("No ID"); ok {
		t.Errorf("Resolve(\"No ID\") = true, want false (missing ID)")
	}
	if _, ok := d.Resolve(""); ok {
		t.Errorf("Resolve(\"\") = true, want false")
	}
}

func TestMentionDirectory_RecordOverwrites(t *testing.T) {
	d := NewMentionDirectory()
	d.Record(Account{ID: "29:old", Name: "Sam"})
	d.Record(Account{ID: "29:new", Name: "Sam"})

	got, ok := d.Resolve("Sam")
	if !ok || got.ID != "29:new" {
		t.Errorf("expected last-write-wins, got %+v ok=%v", got, ok)
	}
}

func TestMentionDirectory_RecordEntities(t *testing.T) {
	d := NewMentionDirectory()
	d.RecordEntities([]Entity{
		{Type: "mention", Mentioned: &Account{ID: "29:bot", Name: "QuillBot"}, Text: "<at>QuillBot</at>"},
		{Type: "mention", Mentioned: &Account{ID: "29:user1", Name: "Neil Kuan"}, Text: "<at>Neil Kuan</at>"},
		{Type: "clientInfo"}, // ignored
		{Type: "mention", Mentioned: nil},                                         // ignored
		{Type: "mention", Mentioned: &Account{ID: "29:x", Name: ""}, Text: "..."}, // ignored, no name
	})

	for _, name := range []string{"QuillBot", "Neil Kuan"} {
		if _, ok := d.Resolve(name); !ok {
			t.Errorf("Resolve(%q) = false, want true", name)
		}
	}
}

func TestMentionDirectory_BuildMentionEntities(t *testing.T) {
	d := NewMentionDirectory()
	d.Record(Account{ID: "29:neil", Name: "Neil Kuan"})
	d.Record(Account{ID: "29:alice", Name: "Alice"})

	tests := []struct {
		name string
		text string
		want []Entity
	}{
		{
			name: "no tags",
			text: "Hello world",
			want: nil,
		},
		{
			name: "single mention",
			text: "Hey <at>Neil Kuan</at> 你看一下這個",
			want: []Entity{
				{Type: "mention", Text: "<at>Neil Kuan</at>", Mentioned: &Account{ID: "29:neil", Name: "Neil Kuan"}},
			},
		},
		{
			name: "two distinct mentions",
			text: "<at>Alice</at> 找 <at>Neil Kuan</at> 一起",
			want: []Entity{
				{Type: "mention", Text: "<at>Alice</at>", Mentioned: &Account{ID: "29:alice", Name: "Alice"}},
				{Type: "mention", Text: "<at>Neil Kuan</at>", Mentioned: &Account{ID: "29:neil", Name: "Neil Kuan"}},
			},
		},
		{
			name: "duplicate same casing — emit once",
			text: "<at>Alice</at> hi <at>Alice</at>",
			want: []Entity{
				{Type: "mention", Text: "<at>Alice</at>", Mentioned: &Account{ID: "29:alice", Name: "Alice"}},
			},
		},
		{
			name: "case-insensitive lookup, preserves original casing in Text",
			text: "<at>alice</at> 你好",
			want: []Entity{
				{Type: "mention", Text: "<at>alice</at>", Mentioned: &Account{ID: "29:alice", Name: "Alice"}},
			},
		},
		{
			name: "unknown name is skipped",
			text: "<at>Bob</at> 不在表中",
			want: nil,
		},
		{
			name: "mixed known and unknown",
			text: "<at>Bob</at> 跟 <at>Alice</at> 確認",
			want: []Entity{
				{Type: "mention", Text: "<at>Alice</at>", Mentioned: &Account{ID: "29:alice", Name: "Alice"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.BuildMentionEntities(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildMentionEntities = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestMentionDirectory_NilSafe(t *testing.T) {
	var d *MentionDirectory
	d.Record(Account{ID: "29:x", Name: "X"})
	d.RecordEntities([]Entity{{Type: "mention", Mentioned: &Account{ID: "29:y", Name: "Y"}}})

	if _, ok := d.Resolve("X"); ok {
		t.Errorf("nil Resolve = true, want false")
	}
	if got := d.BuildMentionEntities("<at>X</at>"); got != nil {
		t.Errorf("nil BuildMentionEntities = %+v, want nil", got)
	}
}

func TestMentionDirectory_ConcurrentAccess(t *testing.T) {
	d := NewMentionDirectory()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Record(Account{ID: "29:user", Name: "User"})
			_, _ = d.Resolve("user")
			_ = d.BuildMentionEntities("<at>User</at>")
		}(i)
	}
	wg.Wait()
}

func TestApplyMentionsAppendsEntities(t *testing.T) {
	h := &Handler{Mentions: NewMentionDirectory()}
	h.Mentions.Record(Account{ID: "29:neil", Name: "Neil Kuan"})

	a := &Activity{
		Text:     "Hi <at>Neil Kuan</at>",
		Entities: []Entity{{Type: "clientInfo"}},
	}
	h.applyMentions(a)

	if len(a.Entities) != 2 {
		t.Fatalf("entities len = %d, want 2 (existing + new mention)", len(a.Entities))
	}
	if a.Entities[0].Type != "clientInfo" {
		t.Errorf("first entity preserved type = %q, want clientInfo", a.Entities[0].Type)
	}
	if a.Entities[1].Type != "mention" || a.Entities[1].Mentioned == nil || a.Entities[1].Mentioned.ID != "29:neil" {
		t.Errorf("appended entity = %+v, want mention for 29:neil", a.Entities[1])
	}
}

func TestApplyMentionsNoTagsLeavesActivityUntouched(t *testing.T) {
	h := &Handler{Mentions: NewMentionDirectory()}
	h.Mentions.Record(Account{ID: "29:neil", Name: "Neil Kuan"})

	a := &Activity{Text: "no mentions here"}
	h.applyMentions(a)
	if a.Entities != nil {
		t.Errorf("entities = %+v, want nil", a.Entities)
	}
}

func TestApplyMentionsNilHandlerOrActivity(t *testing.T) {
	// Nil-safe paths.
	h := &Handler{Mentions: nil}
	a := &Activity{Text: "<at>Neil Kuan</at>"}
	h.applyMentions(a)
	if a.Entities != nil {
		t.Errorf("nil dir entities = %+v, want nil", a.Entities)
	}

	h2 := &Handler{Mentions: NewMentionDirectory()}
	h2.applyMentions(nil) // must not panic
}

// newMockBotClient stands up two httptest servers — one for the
// Microsoft token endpoint, one for the Bot Framework REST API — and
// returns a BotClient wired to both. The handler argument runs against
// the API server (token requests are handled internally).
func newMockBotClient(t *testing.T, handler http.HandlerFunc) (*BotClient, *httptest.Server, func()) {
	t.Helper()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "mock-token",
			"expires_in":   3600,
		})
	}))
	apiServer := httptest.NewServer(handler)
	client := NewBotClient(&BotAuth{appID: "a", appSecret: "s", tenantID: "t", tokenURL: tokenServer.URL})
	return client, apiServer, func() {
		apiServer.Close()
		tokenServer.Close()
	}
}

func TestSeedMembersAsync_RecordsAllMembers(t *testing.T) {
	var calls atomic.Int32
	client, ts, cleanup := newMockBotClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if want := "/v3/conversations/19:abc@thread.tacv2/members"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		_, _ = w.Write([]byte(`[
            {"id":"29:1-paul","name":"Paul Tung","aadObjectId":"guid-paul"},
            {"id":"29:1-neil","name":"Neil Kuan"}
        ]`))
	})
	defer cleanup()

	h := &Handler{Client: client, Mentions: NewMentionDirectory()}
	h.seedMembersAsync(ts.URL, "19:abc@thread.tacv2")

	// Wait up to 1 second for the goroutine to finish populating the
	// directory. Polls every 10ms so the test stays fast under load.
	if !waitForCondition(t, time.Second, func() bool {
		_, ok := h.Mentions.Resolve("Paul Tung")
		return ok
	}) {
		t.Fatalf("Paul Tung never resolved after seeding")
	}
	if _, ok := h.Mentions.Resolve("Neil Kuan"); !ok {
		t.Errorf("Neil Kuan should also be resolvable after seeding")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("API calls = %d, want 1", got)
	}

	// Second call for the same conversation must not refetch.
	h.seedMembersAsync(ts.URL, "19:abc@thread.tacv2")
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Errorf("API calls after second seed = %d, want 1 (cached)", got)
	}
}

func TestSeedMembersAsync_FailureClearsCacheToAllowRetry(t *testing.T) {
	var calls atomic.Int32
	client, ts, cleanup := newMockBotClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"29:1-paul","name":"Paul Tung"}]`))
	})
	defer cleanup()

	h := &Handler{Client: client, Mentions: NewMentionDirectory()}

	h.seedMembersAsync(ts.URL, "conv-1")
	if !waitForCondition(t, time.Second, func() bool { return calls.Load() == 1 }) {
		t.Fatalf("first call never landed")
	}
	// Wait for the failure goroutine to delete the seeded flag.
	time.Sleep(50 * time.Millisecond)

	// Retry should hit the API again — proving the failure cleared the cache.
	h.seedMembersAsync(ts.URL, "conv-1")
	if !waitForCondition(t, time.Second, func() bool { return calls.Load() == 2 }) {
		t.Fatalf("retry after failure never reached the API; calls=%d", calls.Load())
	}
	if !waitForCondition(t, time.Second, func() bool {
		_, ok := h.Mentions.Resolve("Paul Tung")
		return ok
	}) {
		t.Fatalf("Paul Tung should resolve after retry succeeds")
	}
}

func TestSeedMembersAsync_NoOpWithoutPrereqs(t *testing.T) {
	// Nil mentions / nil client / empty IDs must all return without panicking
	// and without scheduling any work that could touch the network.
	(&Handler{Client: nil, Mentions: NewMentionDirectory()}).seedMembersAsync("https://x", "c")
	(&Handler{Client: &BotClient{}, Mentions: nil}).seedMembersAsync("https://x", "c")
	(&Handler{Client: &BotClient{}, Mentions: NewMentionDirectory()}).seedMembersAsync("", "c")
	(&Handler{Client: &BotClient{}, Mentions: NewMentionDirectory()}).seedMembersAsync("https://x", "")
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
