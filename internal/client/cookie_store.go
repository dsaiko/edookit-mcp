package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// cookieFile is the on-disk format for cached session cookies. CapturedAt is
// used to expire the cache after CookieMaxAge regardless of the cookies' own
// attributes (Edookit's session cookies have no Expires set).
type cookieFile struct {
	CapturedAt time.Time      `json:"captured_at"`
	BaseURL    string         `json:"base_url"`
	Cookies    []*http.Cookie `json:"cookies"`
}

// DefaultCookieCachePath returns the standard per-user cache location,
// <UserCacheDir>/edookit-mcp/cookies.json. On macOS that resolves to
// ~/Library/Caches/edookit-mcp/cookies.json.
func DefaultCookieCachePath() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("user cache dir: %w", err)
	}
	return filepath.Join(cache, "edookit-mcp", "cookies.json"), nil
}

// loadCookies reads cached cookies from path and returns them with their age.
// Returns os.ErrNotExist (wrapped) if the file is absent. Mismatched base URL
// or unreadable payload is treated as a cache miss and reported as an error so
// the caller can decide to re-login.
func loadCookies(path, baseURL string) ([]*http.Cookie, time.Duration, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path comes from our own config (env var or UserCacheDir), not external input
	if err != nil {
		return nil, 0, err
	}
	var cf cookieFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, 0, fmt.Errorf("parse %s: %w", path, err)
	}
	if cf.BaseURL != baseURL {
		return nil, 0, fmt.Errorf("cached cookies are for %s, not %s", cf.BaseURL, baseURL)
	}
	if len(cf.Cookies) == 0 {
		return nil, 0, errors.New("cookie file is empty")
	}
	return cf.Cookies, time.Since(cf.CapturedAt), nil
}

// saveCookies writes cookies to path atomically (write-temp then rename) with
// 0600 permissions so the file is readable only by the owner. Parent dirs are
// created on demand with 0700.
func saveCookies(path, baseURL string, cookies []*http.Cookie) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cookie dir: %w", err)
	}
	data, err := json.MarshalIndent(cookieFile{
		CapturedAt: time.Now(),
		BaseURL:    baseURL,
		Cookies:    cookies,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cookies: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// os.Rename atomically replaces the destination on Unix and (since Go 1.5)
	// on Windows via MoveFileEx(MOVEFILE_REPLACE_EXISTING). On older Windows
	// or with certain ACLs the rename can still fail with EEXIST; if it does,
	// fall back to a remove-then-rename which loses atomicity but at least
	// keeps the cache updating across runs.
	if err := os.Rename(tmp, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			_ = os.Remove(tmp)
			return fmt.Errorf("rename %s -> %s (fallback remove failed: %w): %w", tmp, path, removeErr, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
		}
	}
	return nil
}
