package client

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadCookies_RoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cookies.json")
	base := "https://example.test"
	cookies := []*http.Cookie{
		{Name: "PHPSESSID", Value: "abc", Domain: "example.test", Path: "/"},
		{Name: "X-EdooAuthToken", Value: "xyz", Domain: "example.test", Path: "/"},
	}

	if err := saveCookies(path, base, cookies); err != nil {
		t.Fatalf("saveCookies: %v", err)
	}

	got, age, err := loadCookies(path, base)
	if err != nil {
		t.Fatalf("loadCookies: %v", err)
	}
	if age < 0 || age > time.Minute {
		t.Errorf("age = %v, want a small positive duration", age)
	}
	if len(got) != len(cookies) {
		t.Fatalf("got %d cookies, want %d", len(got), len(cookies))
	}
	for i, want := range cookies {
		if got[i].Name != want.Name || got[i].Value != want.Value {
			t.Errorf("cookie %d = %+v, want name=%q value=%q", i, got[i], want.Name, want.Value)
		}
	}
}

func TestLoadCookies_MissingFile(t *testing.T) {
	t.Parallel()

	_, _, err := loadCookies(filepath.Join(t.TempDir(), "absent.json"), "https://x")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v should wrap os.ErrNotExist for callers to switch on", err)
	}
}

func TestLoadCookies_RejectsStaleBaseURL(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cookies.json")
	// Cookies were captured against one site...
	if err := saveCookies(path, "https://old.test", []*http.Cookie{
		{Name: "PHPSESSID", Value: "x", Domain: "old.test", Path: "/"},
	}); err != nil {
		t.Fatalf("saveCookies: %v", err)
	}

	// ...and we now ask to load them for a different one.
	_, _, err := loadCookies(path, "https://new.test")
	if err == nil {
		t.Fatal("expected error when baseURL doesn't match the cached value, got nil")
	}
}

func TestLoadCookies_RejectsEmptyCookieList(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cookies.json")
	if err := saveCookies(path, "https://example.test", nil); err != nil {
		t.Fatalf("saveCookies (empty): %v", err)
	}

	// Defensive: an empty cookie list shouldn't be treated as a valid cache hit.
	_, _, err := loadCookies(path, "https://example.test")
	if err == nil {
		t.Fatal("expected error for empty cookie list, got nil")
	}
}

func TestSaveCookies_AtomicReplace(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cookies.json")
	base := "https://example.test"

	// Write once.
	if err := saveCookies(path, base, []*http.Cookie{{Name: "A", Value: "1"}}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// And again, with different cookies — must atomically replace.
	if err := saveCookies(path, base, []*http.Cookie{{Name: "B", Value: "2"}}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, _, err := loadCookies(path, base)
	if err != nil {
		t.Fatalf("loadCookies: %v", err)
	}
	if len(got) != 1 || got[0].Name != "B" {
		t.Errorf("got %+v, want one cookie named B", got)
	}

	// Ensure no .tmp file was left behind from the rename.
	tmp := path + ".tmp"
	if _, statErr := os.Stat(tmp); statErr == nil {
		t.Errorf("temp file %s still exists after successful rename", tmp)
	}
}

func TestSaveCookies_FilePermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cookies.json")
	if err := saveCookies(path, "https://example.test", []*http.Cookie{{Name: "A", Value: "1"}}); err != nil {
		t.Fatalf("saveCookies: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("file perms = %o, want 0600 (cookies contain session tokens)", mode)
	}
}

func TestDefaultCookieCachePath_Resolves(t *testing.T) {
	t.Parallel()

	p, err := DefaultCookieCachePath()
	if err != nil {
		t.Fatalf("DefaultCookieCachePath: %v", err)
	}
	if p == "" {
		t.Error("expected non-empty default path")
	}
}
