package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// AccountPool manages multiple auth accounts with round-robin selection
// and failover on 429/quota errors. When only one account exists, behaviour
// is identical to the single-account path.
type AccountPool struct {
	mu       sync.RWMutex
	accounts []pooledAccount
	idx      atomic.Uint64
	pinned   string // if non-empty, only use this named account
}

type pooledAccount struct {
	name string
	am   *AuthManager
}

// NewAccountPool loads accounts from a list of auth paths. Each file is
// treated as a separate account. The name is derived from the filename
// (without extension). Paths may include a directory of *.json files.
func NewAccountPool(paths []string, pinnedName string) (*AccountPool, error) {
	var accounts []pooledAccount
	seen := map[string]bool{}
	for _, p := range paths {
		am, err := LoadAuthManager(p)
		if err != nil {
			continue // skip broken accounts
		}
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		name = strings.TrimSuffix(name, ".workbuddy2api-auth")
		if name == "" {
			name = fmt.Sprintf("account-%d", len(accounts))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		accounts = append(accounts, pooledAccount{name: name, am: am})
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no valid account files found")
	}
	if pinnedName != "" {
		found := false
		for _, a := range accounts {
			if a.name == pinnedName {
				accounts = []pooledAccount{a}
				found = true
				break
			}
		}
		if !found {
			names := make([]string, len(accounts))
			for i, a := range accounts {
				names[i] = a.name
			}
			return nil, fmt.Errorf("account %q not found; available: %s", pinnedName, strings.Join(names, ", "))
		}
	}
	return &AccountPool{accounts: accounts, pinned: pinnedName}, nil
}

// Next returns the next account via round-robin.
func (p *AccountPool) Next() *AuthManager {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := uint64(len(p.accounts))
	if n == 0 {
		return nil
	}
	idx := p.idx.Add(1) % n
	return p.accounts[idx].am
}

// Active returns the current account name (the one that would be used).
func (p *AccountPool) Active() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := uint64(len(p.accounts))
	if n == 0 {
		return ""
	}
	idx := (p.idx.Load()) % n
	return p.accounts[idx].name
}

// All returns all account names.
func (p *AccountPool) All() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.accounts))
	for i, a := range p.accounts {
		out[i] = a.name
	}
	return out
}

// Count returns the number of accounts.
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// DiscoverAccountPaths scans the standard account directory for *.json files.
// Returns paths sorted by name for deterministic loading.
func DiscoverAccountPaths() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	dir := filepath.Join(home, ".workbuddy2api", "accounts")
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// LoadAccountPool builds an account pool from:
// 1. The pinned --account flag (if set)
// 2. All files in ~/.workbuddy2api/accounts/*.json
// 3. Fallback to the single ~/.workbuddy2api-auth.json file
func LoadAccountPool(pinned string) (*AccountPool, error) {
	var paths []string

	// Try the accounts directory first
	paths = DiscoverAccountPaths()

	// Always include the default single-file as a fallback
	paths = append(paths, defaultAuthPath())

	pool, err := NewAccountPool(paths, pinned)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// AccountFile returns the authData for the active account (for display).
func (p *AccountPool) AccountFile() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := uint64(len(p.accounts))
	if n == 0 {
		return ""
	}
	idx := (p.idx.Load()) % n
	return p.accounts[idx].am.path
}

// IsQuotaError checks if the upstream response indicates quota exhaustion
// or rate limiting — signals to try the next account.
func IsQuotaError(code int, body string) bool {
	if code == 429 {
		return true
	}
	// WorkBuddy returns code 11115 for quota exhaustion
	if strings.Contains(body, `"code":11115`) || strings.Contains(body, `"code": 11115`) {
		return true
	}
	if strings.Contains(body, "quota") && strings.Contains(body, "exceeded") {
		return true
	}
	return false
}

// Peek returns the current account without advancing the round-robin index.
func (p *AccountPool) Peek() *AuthManager {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := uint64(len(p.accounts))
	if n == 0 {
		return nil
	}
	idx := (p.idx.Load()) % n
	return p.accounts[idx].am
}
