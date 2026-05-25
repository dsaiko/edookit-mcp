package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	// Use os.CreateTemp for a unique name so two concurrent edookit-mcp
	// processes can't clobber each other's in-flight write (which would
	// produce a partial file under the destination path on rename).
	f, err := os.CreateTemp(dir, "cookies-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmp := f.Name()
	// On any failure below we clean up the temp file so we don't accumulate
	// orphans in the cache dir.
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	// os.Rename atomically replaces the destination on Unix and (since Go 1.5)
	// on Windows via MoveFileEx(MOVEFILE_REPLACE_EXISTING). Older Windows or
	// some ACL configurations can still fail with "already exists" — only in
	// THAT case do we fall back to remove-then-rename. Other errors (perms,
	// I/O, disk full) propagate without touching the existing cache: blowing
	// it away would leave the user worse off than just failing the save.
	if err := os.Rename(tmp, path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return fmt.Errorf("rename %s -> %s (fallback remove failed: %w): %w", tmp, path, removeErr, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("rename %s -> %s (after fallback remove): %w", tmp, path, err)
		}
	}
	tmp = "" // rename succeeded; the deferred cleanup must not delete the destination
	return nil
}
