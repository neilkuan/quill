package acp

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type SessionInfo struct {
	ThreadKey    string    `json:"thread_key"`
	SessionID    string    `json:"session_id"`
	CreatedAt    time.Time `json:"created_at"`
	LastActive   time.Time `json:"last_active"`
	MessageCount uint64    `json:"message_count"`
	Alive        bool      `json:"alive"`
	Resumed      bool      `json:"resumed"`
}

type SessionPool struct {
	connections map[string]*AcpConnection
	mu          sync.RWMutex
	command     string
	args        []string
	workingDir  string
	env         map[string]string
	maxSessions int
	store       *SessionStore
}

func (p *SessionPool) WorkingDir() string {
	return p.workingDir
}

// Command returns the agent binary path configured for this pool.
// The sessionpicker package uses this to pick the matching backend
// when listing historical sessions.
func (p *SessionPool) Command() string {
	return p.command
}

func NewSessionPool(command string, args []string, workingDir string, env map[string]string, maxSessions int) *SessionPool {
	store, err := NewSessionStore(workingDir)
	if err != nil {
		slog.Warn("session store unavailable, resume disabled", "error", err)
	}

	return &SessionPool{
		connections: make(map[string]*AcpConnection),
		command:     command,
		args:        args,
		workingDir:  workingDir,
		env:         env,
		maxSessions: maxSessions,
		store:       store,
	}
}

func (p *SessionPool) GetOrCreate(threadID string) error {
	// Check if alive connection exists (read lock)
	p.mu.RLock()
	if conn, ok := p.connections[threadID]; ok && conn.Alive() {
		p.mu.RUnlock()
		return nil
	}
	p.mu.RUnlock()

	// Need to create or rebuild (write lock)
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	wasStale := false
	if conn, ok := p.connections[threadID]; ok {
		if conn.Alive() {
			return nil
		}
		slog.Warn("stale connection, rebuilding", "thread_id", threadID)
		conn.Kill()
		delete(p.connections, threadID)
		wasStale = true
	}

	if len(p.connections) >= p.maxSessions {
		// LRU eviction: kill the least recently used session
		var lruKey string
		var lruTime time.Time
		for key, conn := range p.connections {
			if lruTime.IsZero() || conn.GetLastActive().Before(lruTime) {
				lruKey = key
				lruTime = conn.GetLastActive()
			}
		}
		if lruKey != "" {
			slog.Info("evicting LRU session", "evicted_key", lruKey, "last_active", lruTime)
			p.connections[lruKey].Kill()
			delete(p.connections, lruKey)
		}
	}

	conn, err := SpawnConnection(p.command, p.args, p.workingDir, p.env, threadID)
	if err != nil {
		return err
	}

	if err := conn.Initialize(); err != nil {
		conn.Kill()
		return err
	}

	// Try to resume a previous session if the store has one and agent supports it
	resumed := false
	if p.store != nil && conn.CanLoadSession {
		if oldSessionID := p.store.Lookup(threadID); oldSessionID != "" {
			slog.Info("attempting session resume", "thread_id", threadID, "old_session_id", oldSessionID)
			if err := conn.SessionLoad(oldSessionID, p.workingDir); err != nil {
				slog.Warn("session resume failed, creating new session", "thread_id", threadID, "error", err)
			} else {
				resumed = true
				p.store.Touch(threadID) // update LastActive
			}
		}
	}

	if !resumed {
		sessionID, err := conn.SessionNew(p.workingDir)
		if err != nil {
			conn.Kill()
			return err
		}
		// Persist the new session ID
		if p.store != nil {
			if err := p.store.Save(threadID, sessionID); err != nil {
				slog.Warn("failed to persist session ID, resume will be unavailable", "thread_id", threadID, "error", err)
			}
		}
		// If we had a stale connection, mark as reset so the handler
		// shows "Session expired, starting fresh..."
		if wasStale {
			conn.SessionReset = true
		}
	}

	p.connections[threadID] = conn
	return nil
}

// Connection returns the active connection for a thread, or nil if none
// exists. The returned pointer is safe to retain — connections are not
// mutated after creation, and callers must gate use on conn.Alive().
// Useful when a handler needs to capture the specific connection that
// owns a prompt (e.g. for cancel routing) without holding the pool lock
// across a long-running operation.
//
// Race note: the pool may evict or replace this connection after the
// RLock is released, but that is by design — the returned pointer
// pins the original object (GC keeps it alive while the caller holds
// the pointer), and Kill()/LRU eviction flips alive to false. So a
// later conn.SessionCancel() on a captured stale pointer cleanly
// errors out with "connection not alive" rather than mis-cancelling a
// freshly-spawned connection on the same threadKey.
func (p *SessionPool) Connection(threadKey string) *AcpConnection {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connections[threadKey]
}

// WithConnection provides access to a connection. Caller must have called GetOrCreate first.
func (p *SessionPool) WithConnection(threadID string, fn func(conn *AcpConnection) error) error {
	p.mu.Lock()
	conn, ok := p.connections[threadID]
	if !ok {
		p.mu.Unlock()
		return fmt.Errorf("no connection for thread %s", threadID)
	}
	p.mu.Unlock()
	return fn(conn)
}

func (p *SessionPool) CleanupIdle(ttlSecs int64) {
	cutoff := time.Now().Add(-time.Duration(ttlSecs) * time.Second)

	p.mu.Lock()
	defer p.mu.Unlock()

	var stale []string
	for key, conn := range p.connections {
		if conn.GetLastActive().Before(cutoff) || !conn.Alive() {
			stale = append(stale, key)
		}
	}

	for _, key := range stale {
		slog.Info("cleaning up idle session", "thread_id", key)
		if conn, ok := p.connections[key]; ok {
			conn.Kill()
		}
		delete(p.connections, key)
	}
}

func (p *SessionPool) Shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()

	count := len(p.connections)
	for _, conn := range p.connections {
		conn.Kill()
	}
	p.connections = make(map[string]*AcpConnection)
	slog.Info("pool shutdown complete", "count", count)
}

// ListSessions returns a snapshot of all active sessions.
func (p *SessionPool) ListSessions() []SessionInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	sessions := make([]SessionInfo, 0, len(p.connections))
	for key, conn := range p.connections {
		sessions = append(sessions, SessionInfo{
			ThreadKey:    key,
			SessionID:    conn.SessionID,
			CreatedAt:    conn.CreatedAt,
			LastActive:   conn.GetLastActive(),
			MessageCount: conn.MessageCount.Load(),
			Alive:        conn.Alive(),
			Resumed:      conn.WasResumed,
		})
	}
	return sessions
}

// GetSessionInfo returns metadata for a specific session.
func (p *SessionPool) GetSessionInfo(threadKey string) (*SessionInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, ok := p.connections[threadKey]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", threadKey)
	}
	return &SessionInfo{
		ThreadKey:    threadKey,
		SessionID:    conn.SessionID,
		CreatedAt:    conn.CreatedAt,
		LastActive:   conn.GetLastActive(),
		MessageCount: conn.MessageCount.Load(),
		Alive:        conn.Alive(),
		Resumed:      conn.WasResumed,
	}, nil
}

// KillSession terminates a specific session and removes it from the pool.
// The session store entry is preserved so /resume can find it later.
func (p *SessionPool) KillSession(threadKey string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	conn, ok := p.connections[threadKey]
	if !ok {
		return fmt.Errorf("session not found: %s", threadKey)
	}
	slog.Info("killing session", "thread_key", threadKey)
	conn.Kill()
	delete(p.connections, threadKey)
	return nil
}

// ResetSession terminates a session AND clears its store entry (full fresh start).
func (p *SessionPool) ResetSession(threadKey string) error {
	if err := p.KillSession(threadKey); err != nil {
		return err
	}
	if p.store != nil {
		p.store.Remove(threadKey)
	}
	return nil
}

// ResumeSession explicitly attempts to resume a previous session for the given thread.
// Returns (resumed bool, message string).
// - If no stored session exists: returns false with explanation.
// - If agent doesn't support loadSession: returns false with explanation.
// - If session/load succeeds: returns true.
// - If session/load fails: falls back to session/new and returns false with explanation.
func (p *SessionPool) ResumeSession(threadKey string) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Kill existing connection if any
	if conn, ok := p.connections[threadKey]; ok {
		conn.Kill()
		delete(p.connections, threadKey)
	}

	// Check store for previous session ID
	if p.store == nil {
		return false, "Session store is not available."
	}
	oldSessionID := p.store.Lookup(threadKey)
	if oldSessionID == "" {
		return false, "No previous session found for this thread."
	}

	// Spawn fresh agent process
	conn, err := SpawnConnection(p.command, p.args, p.workingDir, p.env, threadKey)
	if err != nil {
		return false, fmt.Sprintf("Failed to start agent: `%v`", err)
	}

	if err := conn.Initialize(); err != nil {
		conn.Kill()
		return false, fmt.Sprintf("Agent initialization failed: `%v`", err)
	}

	if !conn.CanLoadSession {
		// Agent doesn't support session/load — fall back to new session
		if _, err := conn.SessionNew(p.workingDir); err != nil {
			conn.Kill()
			return false, fmt.Sprintf("Failed to create new session: `%v`", err)
		}
		if err := p.store.Save(threadKey, conn.SessionID); err != nil {
			slog.Warn("failed to persist session ID", "thread_key", threadKey, "error", err)
		}
		p.connections[threadKey] = conn
		return false, fmt.Sprintf("Agent does not support session resume (`loadSession` capability not advertised). A new session has been created.\n\nPrevious session ID was: `%s`", oldSessionID)
	}

	// Attempt session/load
	if err := conn.SessionLoad(oldSessionID, p.workingDir); err != nil {
		slog.Warn("explicit resume failed, falling back to new session",
			"thread_key", threadKey, "old_session_id", oldSessionID, "error", err)
		if _, newErr := conn.SessionNew(p.workingDir); newErr != nil {
			conn.Kill()
			return false, fmt.Sprintf("Resume failed (`%v`) and new session also failed: `%v`", err, newErr)
		}
		if saveErr := p.store.Save(threadKey, conn.SessionID); saveErr != nil {
			slog.Warn("failed to persist session ID", "thread_key", threadKey, "error", saveErr)
		}
		p.connections[threadKey] = conn
		return false, fmt.Sprintf("Could not restore previous session (`%s`): `%v`\n\nA new session has been created.", oldSessionID, err)
	}

	p.store.Touch(threadKey)
	p.connections[threadKey] = conn
	return true, fmt.Sprintf("🔄 Session restored! Continuing conversation from `%s`.", oldSessionID)
}

// LoadSessionForThread kills any existing connection for threadKey,
// spawns a fresh agent process, and asks the agent to load the given
// session ID via ACP session/load. The loaded session takes over the
// thread — subsequent prompts reuse its conversation history.
//
// cwd is the working directory to report in session/load; when empty,
// the pool's default working_dir is used. Picker callers should pass
// the session's original cwd so agents that verify session-to-cwd
// binding (e.g. claude-agent-acp) accept the load.
//
// Behaviour on failure mirrors ResumeSession: if the agent does not
// advertise loadSession, or session/load returns an error, we fall
// back to session/new so the thread is not left in a broken state.
// The returned message tells the caller which branch ran.
func (p *SessionPool) LoadSessionForThread(threadKey, sessionID, cwd string) (bool, string) {
	if sessionID == "" {
		return false, "Session ID is required."
	}
	if cwd == "" {
		cwd = p.workingDir
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Short-circuit: the user picked the same session their current
	// thread is already running. Kiro (and similar) reject a concurrent
	// session/load on the same id because the first process still owns
	// it, so re-loading is both pointless and noisy.
	if conn, ok := p.connections[threadKey]; ok && conn.SessionID == sessionID && conn.Alive() {
		return true, fmt.Sprintf("Session `%s` is already the active session for this thread — nothing to resume.", sessionID)
	}

	if conn, ok := p.connections[threadKey]; ok {
		conn.Kill()
		delete(p.connections, threadKey)
	}

	conn, err := SpawnConnection(p.command, p.args, p.workingDir, p.env, threadKey)
	if err != nil {
		return false, fmt.Sprintf("Failed to start agent: `%v`", err)
	}
	if err := conn.Initialize(); err != nil {
		conn.Kill()
		return false, fmt.Sprintf("Agent initialization failed: `%v`", err)
	}

	if !conn.CanLoadSession {
		if _, err := conn.SessionNew(p.workingDir); err != nil {
			conn.Kill()
			return false, fmt.Sprintf("Agent does not support loadSession and new session failed: `%v`", err)
		}
		if p.store != nil {
			if saveErr := p.store.Save(threadKey, conn.SessionID); saveErr != nil {
				slog.Warn("failed to persist session ID", "thread_key", threadKey, "error", saveErr)
			}
		}
		p.connections[threadKey] = conn
		return false, fmt.Sprintf("Agent does not support session resume (`loadSession` capability not advertised). A fresh session has been started instead of loading `%s`.", sessionID)
	}

	if err := conn.SessionLoad(sessionID, cwd); err != nil {
		slog.Warn("picker session/load failed, falling back to new session",
			"thread_key", threadKey, "session_id", sessionID, "error", err)
		if _, newErr := conn.SessionNew(p.workingDir); newErr != nil {
			conn.Kill()
			return false, fmt.Sprintf("Load failed (`%v`) and new session also failed: `%v`", err, newErr)
		}
		if p.store != nil {
			if saveErr := p.store.Save(threadKey, conn.SessionID); saveErr != nil {
				slog.Warn("failed to persist session ID", "thread_key", threadKey, "error", saveErr)
			}
		}
		p.connections[threadKey] = conn
		// Kiro (and any agent that locks session files per-process) reports
		// `Session is active in another process` when the session is already
		// open elsewhere — another quill instance, a local Kiro TUI, etc.
		// Spell that out so the user knows to close the other client, not
		// that picker is broken.
		friendly := fmt.Sprintf("Could not load session `%s`: `%v`\n\nA fresh session has been started.", sessionID, err)
		if strings.Contains(err.Error(), "active in another process") {
			friendly = fmt.Sprintf("Session `%s` is currently open in another process (local Kiro TUI or another quill instance). Close it there and try again.\n\nA fresh session has been started meanwhile.", sessionID)
		}
		return false, friendly
	}

	if p.store != nil {
		if err := p.store.Save(threadKey, sessionID); err != nil {
			slog.Warn("failed to persist loaded session ID", "thread_key", threadKey, "error", err)
		}
	}
	p.connections[threadKey] = conn
	slog.Info("session loaded via picker", "thread_key", threadKey, "session_id", sessionID)
	return true, fmt.Sprintf("✅ Session `%s` loaded — continue the conversation by sending a message.", sessionID)
}

// CancelSession sends session/cancel to the agent for a specific thread.
// Unlike KillSession, this preserves the connection and session ID — the
// agent stops the active prompt and the pending session/prompt response
// returns with stopReason="cancelled".
// Returns an error if no connection exists for the thread.
func (p *SessionPool) CancelSession(threadKey string) error {
	p.mu.RLock()
	conn, ok := p.connections[threadKey]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no active session for %s", threadKey)
	}
	return conn.SessionCancel()
}

// Stats returns pool utilization.
func (p *SessionPool) Stats() (active int, max int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.connections), p.maxSessions
}
