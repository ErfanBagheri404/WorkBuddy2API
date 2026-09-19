package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	upstreamHost = "www.workbuddy.ai"
	upstreamBase = "https://" + upstreamHost
	platform     = "workbuddy-ai"
	prefixPath   = "/plugin"
)

// knownUpstreamIPs: IPs that have been observed serving workbuddy.ai.
// Used as fallback when DNS flakes (the domain sometimes returns 0.0.0.1).
var knownUpstreamIPs = []string{
	"43.163.17.187",
}

// newHTTPClient creates an HTTP client with a custom resolver that falls
// back to hardcoded IPs when DNS returns 0.0.0.0/0.0.0.1 or fails entirely.
func newHTTPClient(timeout time.Duration) *http.Client {
	innerResolver := &net.Resolver{
		PreferGo: false,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, address)
		},
	}
	resolver := &failingResolver{
		inner:       innerResolver,
		fallbackIPs: knownUpstreamIPs,
		host:        upstreamHost,
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// addr is "host:port"; we let resolver handle host lookup
				host, port, _ := net.SplitHostPort(addr)
				ips, err := resolver.LookupAddr(ctx, host)
				if err != nil || len(ips) == 0 {
					// Fallback to known IPs
					if len(knownUpstreamIPs) > 0 {
						ip := knownUpstreamIPs[rand.Intn(len(knownUpstreamIPs))]
						return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
					}
					return nil, fmt.Errorf("no IPs for %s", host)
				}
				// Pick a random resolved IP
				ip := strings.TrimSuffix(ips[0], ".")
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			},
			TLSClientConfig:     &tls.Config{ServerName: upstreamHost},
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// failingResolver wraps a real resolver and falls back to hardcoded IPs
// when DNS returns loopback/unusable addresses.
type failingResolver struct {
	inner       *net.Resolver
	fallbackIPs []string
	host        string
}

func (r *failingResolver) LookupAddr(ctx context.Context, host string) ([]string, error) {
	// Try real DNS first
	ips, err := r.inner.LookupIPAddr(ctx, host)
	if err == nil {
		result := make([]string, 0, len(ips))
		for _, ip := range ips {
			s := ip.String()
			// Filter out unusable results
			if s == "0.0.0.0" || s == "0.0.0.1" || s == "::" || strings.HasPrefix(s, "127.") {
				continue
			}
			result = append(result, s)
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	// Fallback
	return r.fallbackIPs, nil
}

// doRequest is a low-level helper that adds the Host header when using a
// hardcoded IP so TLS handshake works.
func doRequest(client *http.Client, method, urlStr string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}
	// Parse the URL to extract host
	parsed, err := url.Parse(urlStr)
	if err == nil && parsed.Hostname() == upstreamHost {
		// The Host header must match the TLS ServerName
		req.Host = upstreamHost
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// authData mirrors the token payload stored on disk.
type authData struct {
	Account struct {
		UID string `json:"uid"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Domain       string `json:"domain"`
		LastRefresh  int64  `json:"lastRefreshTime"`
		ExpiresAt    int64  `json:"expiresAt"`
	} `json:"auth"`
}

func defaultAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "workbuddy-auth.json"
	}
	return home + string(os.PathSeparator) + ".workbuddy2api-auth.json"
}

// desktopCredentialPaths returns the WorkBuddy/CodeBuddy desktop credential
// files for the current platform, most-preferred first. The desktop app writes
// the same authData shape we persist, so an already-signed-in install can be
// imported without a browser round-trip.
func desktopCredentialPaths() []string {
	var dirs []string
	switch runtime.GOOS {
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			dirs = append(dirs, filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth"))
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs,
				filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth"))
		}
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			dirs = append(dirs, filepath.Join(xdg, "CodeBuddyExtension", "Data", "Public", "auth"))
		} else if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, ".local", "share", "CodeBuddyExtension", "Data", "Public", "auth"))
		}
	}

	var out []string
	for _, dir := range dirs {
		// Prefer the stable name; fall back to the newest timestamped snapshot.
		stable := filepath.Join(dir, "workbuddy-desktop-ai.info")
		if _, err := os.Stat(stable); err == nil {
			out = append(out, stable)
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "workbuddy-desktop-ai.*.info"))
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		out = append(out, matches...)
	}
	return out
}

// ImportDesktopCredentials looks for an existing desktop sign-in and copies its
// tokens to destPath. Returns the source path, or "" when nothing was found.
// An existing destPath is left untouched unless force is set.
func ImportDesktopCredentials(destPath string, force bool) (string, error) {
	if !force {
		if _, err := os.Stat(destPath); err == nil {
			return "", nil
		}
	}
	for _, src := range desktopCredentialPaths() {
		raw, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		var d authData
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		if d.Auth.AccessToken == "" || d.Auth.RefreshToken == "" {
			continue
		}
		// Re-marshal into our own shape so we never carry unknown desktop
		// fields into the proxy's auth file.
		out, err := json.MarshalIndent(&d, "", "  ")
		if err != nil {
			return "", fmt.Errorf("encode imported credentials: %w", err)
		}
		if err := os.WriteFile(destPath, out, 0600); err != nil {
			return "", fmt.Errorf("write %s: %w", destPath, err)
		}
		return src, nil
	}
	return "", nil
}

// AuthManager holds the WorkBuddy session tokens and refreshes them.
type AuthManager struct {
	mu       sync.Mutex
	path     string
	data     *authData
	client   *http.Client
	uid      string
	domain   string
}

// LoadAuthManager reads stored credentials, refreshing the access token if needed.
func LoadAuthManager(path string) (*AuthManager, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth file %s: %w", path, err)
	}
	var d authData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse auth file: %w", err)
	}
	if d.Auth.AccessToken == "" || d.Auth.RefreshToken == "" {
		return nil, fmt.Errorf("auth file missing tokens")
	}
	am := &AuthManager{path: path, data: &d, client: newHTTPClient(30 * time.Second)}
	am.uid = d.Account.UID
	am.domain = d.Auth.Domain
	if time.Now().UnixMilli() > d.Auth.ExpiresAt-60_000 {
		if err := am.refreshLocked(); err != nil {
			return nil, fmt.Errorf("refresh expired token: %w", err)
		}
		if err := am.saveLocked(); err != nil {
			return nil, err
		}
	}
	return am, nil
}

func (a *AuthManager) saveLocked() error {
	a.data.Account.UID = a.uid
	a.data.Auth.Domain = a.domain
	raw, err := json.MarshalIndent(a.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.path, raw, 0600)
}

// headers returns fresh auth headers for an upstream request.
func (a *AuthManager) headers() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.data.Auth.ExpiresAt-time.Now().UnixMilli() < 5*60_000 {
		if err := a.refreshLocked(); err == nil {
			_ = a.saveLocked()
		}
	}
	h := map[string]string{
		"Authorization": "Bearer " + a.data.Auth.AccessToken,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"X-Product":     "SaaS",
	}
	if a.uid != "" {
		h["X-User-Id"] = a.uid
	}
	if a.domain != "" {
		h["X-Domain"] = a.domain
	}
	return h
}

func (a *AuthManager) doUpstream(method, path string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	u := upstreamBase + path
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Host = upstreamHost
	for k, v := range a.headers() {
		req.Header.Set(k, v)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Request-ID", newRequestID())
	return a.client.Do(req)
}

type refreshResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Domain       string `json:"domain"`
		ExpiresIn    int64  `json:"expiresIn"`
	} `json:"data"`
}

func (a *AuthManager) refreshLocked() error {
	req, err := http.NewRequest(http.MethodPost, upstreamBase+"/v2"+prefixPath+"/auth/token/refresh", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Host = upstreamHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Refresh-Token", a.data.Auth.RefreshToken)
	req.Header.Set("X-Request-ID", newRequestID())
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r refreshResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}
	if r.Code != 0 {
		return fmt.Errorf("refresh failed code=%d msg=%s", r.Code, r.Msg)
	}
	a.data.Auth.AccessToken = r.Data.AccessToken
	if r.Data.RefreshToken != "" {
		a.data.Auth.RefreshToken = r.Data.RefreshToken
	}
	if r.Data.Domain != "" {
		a.domain = r.Data.Domain
		a.data.Auth.Domain = r.Data.Domain
	}
	now := time.Now().UnixMilli()
	a.data.Auth.LastRefresh = now
	if r.Data.ExpiresIn > 0 {
		a.data.Auth.ExpiresAt = now + r.Data.ExpiresIn*1000
	}
	return nil
}

func newRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}
