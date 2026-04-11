package acp

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type SessionPool struct {
	connections map[string]*AcpConnection
	mu          sync.RWMutex
	command     string
	args        []string
	workingDir  string
	env         map[string]string
	maxSessions int
}

func (p *SessionPool) WorkingDir() string {
	return p.workingDir
}

func NewSessionPool(command string, args []string, workingDir string, env map[string]string, maxSessions int) *SessionPool {
	return &SessionPool{
		connections: make(map[string]*AcpConnection),
		command:     command,
		args:        args,
		workingDir:  workingDir,
		env:         env,
		maxSessions: maxSessions,
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
	if conn, ok := p.connections[threadID]; ok {
		if conn.Alive() {
			return nil
		}
		slog.Warn("stale connection, rebuilding", "thread_id", threadID)
		conn.Kill()
		delete(p.connections, threadID)
	}

	if len(p.connections) >= p.maxSessions {
		return fmt.Errorf("pool exhausted (%d sessions)", p.maxSessions)
	}

	conn, err := SpawnConnection(p.command, p.args, p.workingDir, p.env)
	if err != nil {
		return err
	}

	if err := conn.Initialize(); err != nil {
		conn.Kill()
		return err
	}

	if _, err := conn.SessionNew(p.workingDir); err != nil {
		conn.Kill()
		return err
	}

	if _, existed := p.connections[threadID]; existed {
		conn.SessionReset = true
	}

	p.connections[threadID] = conn
	return nil
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
		if conn.LastActive.Before(cutoff) || !conn.Alive() {
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
