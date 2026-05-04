package teams

import (
	"regexp"
	"strings"
	"sync"
)

// mentionPattern matches <at>NAME</at> tags. Names can contain spaces,
// dots, hyphens etc. — we only forbid '<' so the regex stops at the
// closing tag.
var mentionPattern = regexp.MustCompile(`<at>([^<]+)</at>`)

// MentionDirectory maps display names to Teams accounts so the bot can
// turn agent-generated <at>Name</at> tags back into Bot Framework
// mention entities. Populated lazily from incoming activities — a name
// becomes resolvable only after that user has spoken in (or been
// mentioned in) a channel the bot saw.
type MentionDirectory struct {
	mu    sync.RWMutex
	byKey map[string]Account
}

func NewMentionDirectory() *MentionDirectory {
	return &MentionDirectory{byKey: make(map[string]Account)}
}

// Record stores `acc` keyed by its normalized display name. Entries
// without a usable Bot Framework ID (`29:xxx`) or display name are
// dropped — both are required to compose a working mention entity.
func (d *MentionDirectory) Record(acc Account) {
	if d == nil {
		return
	}
	name := strings.TrimSpace(acc.Name)
	id := strings.TrimSpace(acc.ID)
	if name == "" || id == "" {
		return
	}
	key := normalizeMentionKey(name)
	d.mu.Lock()
	d.byKey[key] = Account{ID: id, Name: name, AADObjectID: acc.AADObjectID}
	d.mu.Unlock()
}

// RecordEntities walks `entities` and records every `mention` whose
// `mentioned` block carries usable id+name pairs. This is how we learn
// about *other* people from incoming messages — Teams emits a `mention`
// entity for each @-tagged user, including the bot itself, so other
// users become resolvable as soon as someone @-mentions them.
func (d *MentionDirectory) RecordEntities(entities []Entity) {
	if d == nil {
		return
	}
	for _, e := range entities {
		if e.Type != "mention" || e.Mentioned == nil {
			continue
		}
		d.Record(*e.Mentioned)
	}
}

// Resolve looks up an account by display name (case-insensitive,
// whitespace-trimmed).
func (d *MentionDirectory) Resolve(name string) (Account, bool) {
	if d == nil {
		return Account{}, false
	}
	key := normalizeMentionKey(name)
	d.mu.RLock()
	defer d.mu.RUnlock()
	acc, ok := d.byKey[key]
	return acc, ok
}

// BuildMentionEntities scans `text` for <at>Name</at> tags, resolves
// each unique name against the directory, and returns the matching Bot
// Framework mention entities. Names not in the directory are skipped —
// the literal <at>Name</at> stays in the rendered text so the message
// remains readable even when the mention can't be wired up.
//
// The Entity.Text field carries the original substring (preserving
// casing) because Bot Framework matches on exact textual equality.
func (d *MentionDirectory) BuildMentionEntities(text string) []Entity {
	if d == nil || text == "" {
		return nil
	}
	matches := mentionPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var entities []Entity
	for _, idx := range matches {
		full := text[idx[0]:idx[1]]
		name := strings.TrimSpace(text[idx[2]:idx[3]])
		if name == "" {
			continue
		}
		dedupeKey := full + "|" + normalizeMentionKey(name)
		if seen[dedupeKey] {
			continue
		}
		seen[dedupeKey] = true
		acc, ok := d.Resolve(name)
		if !ok {
			continue
		}
		entities = append(entities, Entity{
			Type: "mention",
			Text: full,
			Mentioned: &Account{
				ID:   acc.ID,
				Name: acc.Name,
			},
		})
	}
	return entities
}

func normalizeMentionKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
