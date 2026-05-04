package teams

import (
	"reflect"
	"sync"
	"testing"
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
