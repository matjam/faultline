package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Approvals tracks pending MCP config changes and raw collaborator approvals.
type Approvals struct {
	mu      sync.Mutex
	nextID  int
	pending map[string]pendingApproval
}

type pendingApproval struct {
	Hash     string
	Granted  bool
	Used     bool
	Deadline time.Time
}

// NewApprovals creates an approval tracker.
func NewApprovals() *Approvals {
	return &Approvals{pending: make(map[string]pendingApproval)}
}

// Propose stores a pending approval for config and returns the approval text.
func (a *Approvals) Propose(config Config) (id, hash, approvalText string, err error) {
	hash, err = ConfigHash(config)
	if err != nil {
		return "", "", "", err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id = fmt.Sprintf("mcp-%d", a.nextID)
	a.pending[id] = pendingApproval{Hash: hash, Deadline: time.Now().Add(15 * time.Minute)}
	return id, hash, fmt.Sprintf("APPROVE MCP %s %s", id, hash), nil
}

// RecordRaw records approval only from raw collaborator text.
func (a *Approvals) RecordRaw(text string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) != 4 || fields[0] != "APPROVE" || fields[1] != "MCP" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[fields[2]]
	if !ok || p.Used || time.Now().After(p.Deadline) || p.Hash != fields[3] {
		return
	}
	p.Granted = true
	a.pending[fields[2]] = p
}

// Consume grants one update when id was approved for the exact config hash.
func (a *Approvals) Consume(id string, config Config) bool {
	hash, err := ConfigHash(config)
	if err != nil {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[id]
	if !ok || !p.Granted || p.Used || time.Now().After(p.Deadline) || p.Hash != hash {
		return false
	}
	p.Used = true
	a.pending[id] = p
	return true
}

// ConfigHash returns a stable hash for an MCP config proposal.
func ConfigHash(config Config) (string, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
