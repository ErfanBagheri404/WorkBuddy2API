package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type authStateResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data  struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	} `json:"data"`
}

type authTokenResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data  *struct {
		AccessToken   string `json:"accessToken"`
		RefreshToken  string `json:"refreshToken"`
		Domain        string `json:"domain"`
		ExpiresIn     int64  `json:"expiresIn"`
		TokenType     string `json:"tokenType"`
	} `json:"data"`
}

type accountResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data  *struct {
		UID string `json:"uid"`
	} `json:"data"`
}

var loginClient = newHTTPClient(30 * time.Second)

// runLogin performs the OAuth device flow: prints a URL, user logs in,
// polls until tokens arrive.
func runLogin(authPath string) error {
	state, authURL, err := fetchAuthState()
	if err != nil {
		return err
	}

	fmt.Println("Open this URL in your browser to log in with WorkBuddy:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("Waiting for login (press Ctrl+C to cancel)...")
	openBrowser(authURL)

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	timeout := time.After(10 * time.Minute)

	for {
		select {
		case <-timeout:
			return fmt.Errorf("login timed out after 10 minutes")
		case <-ticker.C:
			tok, retry, err := pollAuthToken(state)
			if err != nil {
				return err
			}
			if retry {
				continue
			}
			uid, err := fetchAccountUID(tok.Data.AccessToken)
			if err != nil {
				return fmt.Errorf("fetch account: %w", err)
			}
			if err := saveCredentials(authPath, uid, tok); err != nil {
				return err
			}
			fmt.Printf("\nLogged in! Credentials saved to %s\n", authPath)
			return nil
		}
	}
}

func loginHeaders() http.Header {
	return http.Header{
		"Content-Type":          {"application/json"},
		"X-Product":             {"SaaS"},
		"X-No-Authorization":    {"true"},
		"X-No-User-Id":          {"true"},
		"X-No-Enterprise-Id":    {"true"},
		"X-No-Department-Info":  {"true"},
		"X-Request-ID":          {newRequestID()},
	}
}

func fetchAuthState() (string, string, error) {
	req, _ := http.NewRequest(http.MethodPost, upstreamBase+"/v2"+prefixPath+"/auth/state?platform="+platform, bytes.NewReader([]byte("{}")))
	req.Header = loginHeaders()
	resp, err := loginClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var s authStateResponse
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", "", fmt.Errorf("decode auth state: %w", err)
	}
	if s.Code != 0 || s.Data.State == "" {
		return "", "", fmt.Errorf("auth state failed code=%d msg=%s", s.Code, s.Msg)
	}
	return s.Data.State, s.Data.AuthURL, nil
}

// pollAuthToken returns (token, retry, error).
// Code 11217 means "login ing..." — keep polling.
func pollAuthToken(state string) (*authTokenResponse, bool, error) {
	req, _ := http.NewRequest(http.MethodGet, upstreamBase+"/v2"+prefixPath+"/auth/token?state="+state, nil)
	req.Header = loginHeaders()
	resp, err := loginClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var t authTokenResponse
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, false, fmt.Errorf("decode auth token: %w", err)
	}
	if t.Code == 11217 {
		fmt.Print(".")
		return nil, true, nil
	}
	if t.Code != 0 || t.Data == nil || t.Data.AccessToken == "" {
		return nil, false, fmt.Errorf("login failed code=%d msg=%s", t.Code, t.Msg)
	}
	return &t, false, nil
}

func fetchAccountUID(accessToken string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, upstreamBase+"/v2"+prefixPath+"/accounts", nil)
	req.Host = upstreamHost
	req.Header = http.Header{
		"Content-Type":  {"application/json"},
		"X-Product":     {"SaaS"},
		"Authorization": {"Bearer " + accessToken},
		"X-Request-ID":  {newRequestID()},
	}
	resp, err := loginClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// Response wraps data in { code: 0, data: { accounts: [...] } }
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Accounts []struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"accounts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("decode accounts: %w", err)
	}
	if envelope.Code != 0 {
		return "", fmt.Errorf("accounts failed code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	if len(envelope.Data.Accounts) == 0 {
		return "", fmt.Errorf("no accounts returned")
	}
	return envelope.Data.Accounts[0].UID, nil
}

func saveCredentials(path, uid string, tok *authTokenResponse) error {
	d := &authData{}
	d.Account.UID = uid
	d.Auth.AccessToken = tok.Data.AccessToken
	d.Auth.RefreshToken = tok.Data.RefreshToken
	d.Auth.Domain = tok.Data.Domain
	now := time.Now().UnixMilli()
	d.Auth.LastRefresh = now
	if tok.Data.ExpiresIn > 0 {
		d.Auth.ExpiresAt = now + tok.Data.ExpiresIn*1000
	} else {
		d.Auth.ExpiresAt = now + 365*24*3600*1000
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func openBrowser(url string) {
	for _, cmd := range [][]string{
		{"rundll32", "url.dll,FileProtocolHandler", url}, // Windows
		{"open", url},                                    // macOS
		{"xdg-open", url},                                // Linux
	} {
		if err := startCmd(cmd[0], cmd[1:]...); err == nil {
			return
		}
	}
	fmt.Fprintln(os.Stderr, "(could not auto-open browser; copy the URL manually)")
	_ = bufio.NewReader(os.Stdin)
}
