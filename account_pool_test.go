package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAccount writes a minimal valid auth file so LoadAuthManager accepts it.
func writeAccount(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	// 40-char token, far-future expiry, so the file parses and is not "expired".
	// Both tokens required by LoadAuthManager; far-future expiry so no refresh is attempted.
	body := `{"auth":{"accessToken":"` + fakeTok() + `","refreshToken":"` + fakeTok() + `","domain":"test","expiresAt":4102444800000},"account":{"uid":"u-` + name + `"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeTok() string {
	b := make([]byte, 40)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// TestAccountPool_RoundRobin verifies selection cycles through every account.
func TestAccountPool_RoundRobin(t *testing.T) {
	dir := t.TempDir()
	names := []string{"a.json", "b.json", "c.json"}
	for _, n := range names {
		writeAccount(t, dir, n)
	}

	pool, err := NewAccountPool(pathsFor(dir), "")
	if err != nil {
		t.Fatalf("NewAccountPool: %v", err)
	}
	if got := pool.Count(); got != len(names) {
		t.Fatalf("Count() = %d, want %d", got, len(names))
	}

	seen := map[string]bool{}
	for i := 0; i < len(names)*2; i++ {
		am := pool.Next()
		if am == nil {
			t.Fatal("Next() returned nil")
		}
		seen[filepath.Base(am.path)] = true
	}
	for _, n := range names {
		if !seen[n] {
			t.Errorf("account %s never selected", n)
		}
	}
}

func pathsFor(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return matches
}

// TestAccountPool_PinByName verifies --account restricts the pool to one entry.
func TestAccountPool_PinByName(t *testing.T) {
	dir := t.TempDir()
	writeAccount(t, dir, "a.json")
	writeAccount(t, dir, "b.json")

	pool, err := NewAccountPool(pathsFor(dir), "b")
	if err != nil {
		t.Fatalf("NewAccountPool pin: %v", err)
	}
	if pool.Count() != 1 {
		t.Fatalf("pinned Count() = %d, want 1", pool.Count())
	}
	if got := pool.Active(); got != "b" {
		t.Fatalf("Active() = %q, want b", got)
	}
}

// TestAccountPool_PinMissing verifies an unknown name is an error, not a silent
// fallback to some other account.
func TestAccountPool_PinMissing(t *testing.T) {
	dir := t.TempDir()
	writeAccount(t, dir, "a.json")

	if _, err := NewAccountPool(pathsFor(dir), "nope"); err == nil {
		t.Fatal("expected error for unknown account name")
	}
}

func TestIsQuotaError(t *testing.T) {
	cases := []struct {
		code int
		body string
		want bool
	}{
		{429, "", true},
		{200, "", false},
		{400, `{"code":11115}`, true},
		{400, `"code": 11115`, true},
		{400, "quota exceeded", true},
		{400, "bad request", false},
	}
	for _, c := range cases {
		if got := IsQuotaError(c.code, c.body); got != c.want {
			t.Errorf("IsQuotaError(%d, %q) = %v, want %v", c.code, c.body, got, c.want)
		}
	}
}
