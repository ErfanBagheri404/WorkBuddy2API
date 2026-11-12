package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// setTestHome points os.UserHomeDir() at dir on every platform. Go reads $HOME
// on unix, %USERPROFILE% on windows and $home on plan9; CI runs on
// windows-latest, so setting only HOME silently tests the runner's real home.
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("home", dir)
}

// --- helpers ---------------------------------------------------------------

type scriptedTransport struct {
	status int
	body   string
	hits   *int32
}

func (s *scriptedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if s.hits != nil {
		atomic.AddInt32(s.hits, 1)
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

// fakeAuth builds an AuthManager whose upstream calls never leave the process.
func fakeAuth(uid string, rt http.RoundTripper) *AuthManager {
	d := &authData{}
	d.Account.UID = uid
	d.Auth.AccessToken = "access-token"
	d.Auth.RefreshToken = "refresh-token"
	d.Auth.ExpiresAt = 4102444800000 // year 2100: never refresh
	return &AuthManager{
		data:   d,
		uid:    uid,
		path:   "/tmp/fake-" + uid,
		client: &http.Client{Transport: rt},
	}
}

func quotaBody() string    { return `{"code":11115,"msg":"quota exceeded"}` }
func okModelsBody() string { return `{"code":0,"data":{"models":[{"id":"m1","name":"Model One"}]}}` }

func resetModelCache() {
	globalModelCache.mu.Lock()
	globalModelCache.models = nil
	globalModelCache.fetched = time.Time{}
	globalModelCache.mu.Unlock()
}

func poolOf(ams ...*AuthManager) *AccountPool {
	accounts := make([]pooledAccount, len(ams))
	for i, am := range ams {
		accounts[i] = pooledAccount{name: am.UID(), am: am}
	}
	return &AccountPool{accounts: accounts}
}

// forceStart sets the cursor so the next Order() call begins at index `start`.
func forceStart(p *AccountPool, start int) {
	n := uint64(p.Count())
	if n == 0 {
		return
	}
	p.idx.Store((uint64(start) + n - 1) % n)
}

// --- finding 3: per-operation account order --------------------------------

func TestAccountPool_OrderReturnsEveryAccountOnce(t *testing.T) {
	a := fakeAuth("a", &scriptedTransport{status: 200, body: okModelsBody()})
	b := fakeAuth("b", &scriptedTransport{status: 200, body: okModelsBody()})
	c := fakeAuth("c", &scriptedTransport{status: 200, body: okModelsBody()})
	p := poolOf(a, b, c)
	forceStart(p, 0)

	order := p.Order()
	if len(order) != 3 {
		t.Fatalf("Order() len = %d, want 3", len(order))
	}
	seen := map[string]bool{}
	for _, am := range order {
		if seen[am.UID()] {
			t.Fatalf("account %s appears twice in one order", am.UID())
		}
		seen[am.UID()] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !seen[want] {
			t.Errorf("account %s missing from order", want)
		}
	}
	if order[0].UID() != "a" {
		t.Fatalf("forced start did not put a first, got %s", order[0].UID())
	}
}

func TestAccountPool_OrderRotatesOnEachCall(t *testing.T) {
	a := fakeAuth("a", &scriptedTransport{status: 200, body: okModelsBody()})
	b := fakeAuth("b", &scriptedTransport{status: 200, body: okModelsBody()})
	p := poolOf(a, b)
	forceStart(p, 0)

	first := p.Order()[0].UID()
	second := p.Order()[0].UID()
	if first == second {
		t.Fatalf("consecutive Order() calls started at the same account %q", first)
	}
}

func TestAccountPool_OrderDistributesConcurrentStarts(t *testing.T) {
	ams := []*AuthManager{
		fakeAuth("a", &scriptedTransport{status: 200, body: okModelsBody()}),
		fakeAuth("b", &scriptedTransport{status: 200, body: okModelsBody()}),
		fakeAuth("c", &scriptedTransport{status: 200, body: okModelsBody()}),
	}
	p := poolOf(ams...)

	const calls = 60
	starts := make(chan string, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			starts <- p.Order()[0].UID()
		}()
	}
	wg.Wait()
	close(starts)

	seen := map[string]bool{}
	for s := range starts {
		seen[s] = true
	}
	if len(seen) != 3 {
		t.Fatalf("concurrent operations started on only %d of 3 accounts: %v", len(seen), seen)
	}
}

// --- tryAccounts: local failover order -------------------------------------

func TestTryAccounts_VisitsEachAccountOnceOnQuotaErrors(t *testing.T) {
	a := fakeAuth("a", &scriptedTransport{status: 200, body: okModelsBody()})
	b := fakeAuth("b", &scriptedTransport{status: 200, body: okModelsBody()})
	c := fakeAuth("c", &scriptedTransport{status: 200, body: okModelsBody()})
	order := []*AuthManager{a, b, c}

	var visited []string
	call := func(am *AuthManager) (*http.Response, error) {
		visited = append(visited, am.UID())
		if am != c {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(quotaBody())), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	}

	resp, am, err := tryAccounts(order, call)
	if err != nil {
		t.Fatalf("tryAccounts: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if am != c {
		t.Fatalf("serving account = %s, want c", am.UID())
	}
	if strings.Join(visited, ",") != "a,b,c" {
		t.Fatalf("visited %v, want [a b c]", visited)
	}
}

func TestTryAccounts_ReturnsServingAccountOnSuccess(t *testing.T) {
	a := fakeAuth("a", &scriptedTransport{status: 200, body: okModelsBody()})
	b := fakeAuth("b", &scriptedTransport{status: 200, body: okModelsBody()})
	call := func(am *AuthManager) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	}
	_, am, err := tryAccounts([]*AuthManager{a, b}, call)
	if err != nil {
		t.Fatalf("tryAccounts: %v", err)
	}
	if am != a {
		t.Fatalf("serving account = %s, want a", am.UID())
	}
}

func TestTryAccounts_ConcurrentFailoversUseIndependentOrders(t *testing.T) {
	a := fakeAuth("a", &scriptedTransport{status: 200, body: okModelsBody()})
	b := fakeAuth("b", &scriptedTransport{status: 200, body: okModelsBody()})
	c := fakeAuth("c", &scriptedTransport{status: 200, body: okModelsBody()})

	var mu sync.Mutex
	var visited1, visited2 []string
	quota := func() (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(quotaBody())), Header: make(http.Header)}, nil
	}
	call1 := func(am *AuthManager) (*http.Response, error) {
		mu.Lock()
		visited1 = append(visited1, am.UID())
		mu.Unlock()
		time.Sleep(time.Millisecond)
		return quota()
	}
	call2 := func(am *AuthManager) (*http.Response, error) {
		mu.Lock()
		visited2 = append(visited2, am.UID())
		mu.Unlock()
		time.Sleep(time.Millisecond)
		return quota()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); tryAccounts([]*AuthManager{a, b}, call1) }()
	go func() { defer wg.Done(); tryAccounts([]*AuthManager{c}, call2) }()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(visited1, ",") != "a,b" {
		t.Fatalf("request 1 visited %v, want [a b]", visited1)
	}
	if strings.Join(visited2, ",") != "c" {
		t.Fatalf("request 2 visited %v, want [c]", visited2)
	}
}

// --- finding 1: pool is used even for a single / pinned account -------------

func TestSetTestHome_RedirectsUserHomeDir(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)
	if got, err := os.UserHomeDir(); err != nil || got != dir {
		t.Fatalf("os.UserHomeDir() = %q, %v; want %q (defaultAuthPath and DiscoverAccountPaths depend on it)", got, err, dir)
	}
}

func TestNewClientForAuth_UsesPoolForSinglePinnedAccount(t *testing.T) {
	dir := t.TempDir()
	accountsDir := filepath.Join(dir, ".workbuddy2api", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Only a pool account exists; there is deliberately NO default auth file.
	writeAccount(t, accountsDir, "pinned.json")

	setTestHome(t, dir)
	old := optAccount
	optAccount = "pinned"
	defer func() { optAccount = old }()

	client, am, err := newClientForAuth(defaultAuthPath())
	if err != nil {
		t.Fatalf("newClientForAuth with a valid pinned account failed: %v", err)
	}
	if client == nil || client.pool == nil {
		t.Fatal("expected a pool-backed client for the pinned account")
	}
	if am == nil || am.UID() != "u-pinned.json" {
		t.Fatalf("serving account = %v, want u-pinned.json", am)
	}
}

func TestNewClientForAuth_UsesPoolFileWhenDefaultAuthIsAbsent(t *testing.T) {
	dir := t.TempDir()
	accountsDir := filepath.Join(dir, ".workbuddy2api", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAccount(t, accountsDir, "only.json")

	setTestHome(t, dir)
	old := optAccount
	optAccount = ""
	defer func() { optAccount = old }()

	client, _, err := newClientForAuth(defaultAuthPath())
	if err != nil {
		t.Fatalf("newClientForAuth should start from the pool file: %v", err)
	}
	if client == nil || client.pool == nil {
		t.Fatal("expected a pool-backed client when only a pool file exists")
	}
}

// --- finding 4: model fetch fails over across the pool ---------------------

func TestFetchModelsFor_FailsOverWhenFirstAccountQuotaLimited(t *testing.T) {
	resetModelCache()
	defer resetModelCache()

	var hitsA, hitsB int32
	a := fakeAuth("a", &scriptedTransport{status: 429, body: quotaBody(), hits: &hitsA})
	b := fakeAuth("b", &scriptedTransport{status: 200, body: okModelsBody(), hits: &hitsB})
	p := poolOf(a, b)
	forceStart(p, 0) // a first, so b can only be reached via failover
	c := NewUpstreamClientPool(p)

	models, am, err := FetchModelsFor(c)
	if err != nil {
		t.Fatalf("FetchModelsFor: %v", err)
	}
	if am == nil || am.UID() != "b" {
		t.Fatalf("serving account = %v, want b", am)
	}
	if len(models) == 0 {
		t.Fatal("no models returned")
	}
	if atomic.LoadInt32(&hitsA) != 1 || atomic.LoadInt32(&hitsB) != 1 {
		t.Fatalf("expected one attempt per account, got a=%d b=%d", hitsA, hitsB)
	}
}

// --- finding 2: the operation reports the account that served it ------------

func TestChat_ReturnsServingAccount(t *testing.T) {
	a := fakeAuth("single", &scriptedTransport{status: 200, body: "data: [DONE]\n\n"})
	c := NewUpstreamClient(a)
	resp, am, err := c.Chat(context.Background(), []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	if am == nil || am.UID() != "single" {
		t.Fatalf("serving account = %v, want single", am)
	}
}

func TestChat_PoolReturnsAccountThatServedAfterFailover(t *testing.T) {
	a := fakeAuth("a", &scriptedTransport{status: 429, body: quotaBody()})
	b := fakeAuth("b", &scriptedTransport{status: 200, body: "data: [DONE]\n\n"})
	p := poolOf(a, b)
	forceStart(p, 0)
	c := NewUpstreamClientPool(p)

	resp, am, err := c.Chat(context.Background(), []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer resp.Body.Close()
	if am == nil || am.UID() != "b" {
		t.Fatalf("serving account = %v, want b (a was quota-limited)", am)
	}
}

// --- finding 2: handler records verdicts under the serving account ---------

func TestHandleChat_RecordsVerdictUnderServingAccount(t *testing.T) {
	// Account a is quota-limited, b actually serves the request. The verdict
	// from b's error must be keyed to b, not to the startup snapshot (a).
	a := fakeAuth("acct-a", &scriptedTransport{status: 429, body: quotaBody()})
	bBody := `{"code":11158,"msg":"model [m] is not allowed for user type [free]"}`
	b := fakeAuth("acct-b", &scriptedTransport{status: 400, body: bBody})
	p := poolOf(a, b)
	forceStart(p, 0)
	client := NewUpstreamClientPool(p)

	// Isolate the availability store so we can inspect what got recorded.
	t.Setenv(probeEnv, "0") // no background probe goroutine may outlive this test
	old := modelAvailability
	modelAvailability = &availabilityStore{path: filepath.Join(t.TempDir(), "availability.json")}
	defer func() { modelAvailability = old }()

	srv := NewServer(client, a, "", false, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	modelAvailability.mu.Lock()
	defer modelAvailability.mu.Unlock()
	modelAvailability.loadLocked()
	if got := modelAvailability.data["acct-b"]["m"].Status; got != modelStatusDenied {
		t.Fatalf("verdict for serving account acct-b = %q, want %q; data=%v", got, modelStatusDenied, modelAvailability.data)
	}
	if _, present := modelAvailability.data["acct-a"]["m"]; present {
		t.Fatalf("verdict wrongly recorded under the startup account acct-a: %v", modelAvailability.data)
	}
}

// --- finding 4: /v1/models fails over across the pool ----------------------

func TestHandleModels_UsesPooledFetchWithFailover(t *testing.T) {
	resetModelCache()
	defer resetModelCache()

	var hitsA, hitsB int32
	a := fakeAuth("acct-a", &scriptedTransport{status: 429, body: quotaBody(), hits: &hitsA})
	b := fakeAuth("acct-b", &scriptedTransport{status: 200, body: okModelsBody(), hits: &hitsB})
	p := poolOf(a, b)
	forceStart(p, 0) // a first; only b has a healthy model response
	client := NewUpstreamClientPool(p)

	// Disable background probing so no goroutine keeps reading the global
	// availability store while this test swaps and restores it.
	t.Setenv(probeEnv, "0")
	old := modelAvailability
	modelAvailability = &availabilityStore{path: filepath.Join(t.TempDir(), "availability.json")}
	defer func() { modelAvailability = old }()

	srv := NewServer(client, a, "", false, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Data) == 0 {
		t.Fatal("no models advertised; failover did not reach the healthy account")
	}
	if atomic.LoadInt32(&hitsA) != 1 || atomic.LoadInt32(&hitsB) != 1 {
		t.Fatalf("expected exactly one attempt per account, got a=%d b=%d", hitsA, hitsB)
	}
}
