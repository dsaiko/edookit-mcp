package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

// DownloadResult is what DownloadAttachments returns: one entry per
// attachment Edookit listed on the message, ordered as the message presents
// them. A non-empty Error on an entry means that single download failed
// (other entries continued); a top-level error from DownloadAttachments
// itself means the call couldn't even start (bad ID, can't create the
// destination dir, etc.).
type DownloadResult struct {
	MessageID string           `json:"message_id"`
	Directory string           `json:"directory"`
	Files     []DownloadedFile `json:"files"`
}

// DownloadedFile is one attachment after we attempted to fetch it.
// Bytes is the actual on-disk size. Skipped is true when an existing file
// at the destination path was left alone (only possible with
// Overwrite=false — the call doesn't compare sizes, just presence). Name
// is Edookit's original filename even when Path was disambiguated by a
// suffix to avoid colliding with an earlier attachment in the same call.
type DownloadedFile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// DownloadOptions controls DownloadAttachments. DestDir is the target
// directory; an empty value resolves to <os.TempDir>/edookit-mcp/<message-id>,
// which lands in the OS-appropriate temp location (/tmp on Unix,
// %TMP%\... on Windows). Tilde-expansion is applied to non-empty paths,
// and the directory tree is created on demand with 0700 perms (the files
// inside come from a private school account).
type DownloadOptions struct {
	DestDir   string
	Overwrite bool // if false, an existing file at the destination path is left untouched (no size check — we have no Content-Length up-front)
}

// DownloadAttachments resolves the given message ID, downloads every
// (non-trashed) attachment via the authenticated session, and writes each
// to DestDir/<original-filename>. Returns a per-file outcome. Partial
// failures don't abort the loop — each attachment's outcome is captured in
// its DownloadedFile entry so a single broken file doesn't lose the rest.
func DownloadAttachments(ctx context.Context, cli *client.Client, messageID string, opts DownloadOptions) (*DownloadResult, error) {
	msg, err := GetMessage(ctx, cli, messageID)
	if err != nil {
		return nil, err
	}

	destDir, err := resolveDestDir(opts.DestDir, msg.ID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, fmt.Errorf("create destination dir %s: %w", destDir, err)
	}

	res := &DownloadResult{
		MessageID: msg.ID,
		Directory: destDir,
		Files:     make([]DownloadedFile, 0, len(msg.Attachments)),
	}

	// usedNames tracks final base names already taken inside this call so a
	// second attachment with the same Edookit-supplied filename gets a
	// "-2" / "-3" suffix instead of being silently skipped (Overwrite=false)
	// or clobbering the first (Overwrite=true).
	usedNames := make(map[string]int, len(msg.Attachments))
	for _, a := range msg.Attachments {
		entry := downloadOne(ctx, cli, a, destDir, opts.Overwrite, usedNames)
		res.Files = append(res.Files, entry)
	}
	return res, nil
}

// downloadOne handles a single attachment end-to-end. Returns the result
// entry; never panics. Logs at debug-level for the operator. usedNames is
// shared across the whole DownloadAttachments call so duplicate filenames
// within one message get disambiguated with a "-N" suffix instead of
// silently colliding.
func downloadOne(ctx context.Context, cli *client.Client, a Attachment, destDir string, overwrite bool, usedNames map[string]int) DownloadedFile {
	out := DownloadedFile{Name: a.Name}

	if errMsg := validateAttachment(a); errMsg != "" {
		out.Error = errMsg
		return out
	}

	dst, errMsg := planDestination(a.Name, destDir, usedNames)
	if errMsg != "" {
		out.Error = errMsg
		return out
	}
	out.Path = dst

	return streamToDest(ctx, cli, a.URL, destDir, dst, overwrite, out)
}

// validateAttachment performs the cheap up-front checks that don't depend
// on the destination directory: a present download URL, and a base name
// that isn't a directory-traversal sentinel or Windows-reserved. Returns
// an error message string (empty = OK) so the caller can stuff it into
// the result entry without an error allocation.
func validateAttachment(a Attachment) string {
	// Server-supplied URL validation: an empty / whitespace-only link
	// would otherwise resolve through Client.resolve("") to the base URL
	// itself, so we'd happily save the Edookit landing page as the
	// "attachment". Fail per-file instead.
	if strings.TrimSpace(a.URL) == "" {
		return "attachment has no download URL"
	}
	safeName := filepath.Base(a.Name)
	if safeName == "" || safeName == "." || safeName == ".." || safeName == string(os.PathSeparator) {
		return fmt.Sprintf("attachment name %q is not safe to use as a filename", a.Name)
	}
	if reason := windowsUnsafeName(safeName); reason != "" {
		return fmt.Sprintf("attachment name %q rejected: %s", a.Name, reason)
	}
	return ""
}

// planDestination computes the final on-disk path for an attachment,
// applying within-call filename deduplication and verifying the result
// stays inside destDir. Returns (dst, "") on success or ("", errMsg) if
// the name can't be placed safely. Mutates usedNames to claim the chosen
// filename so subsequent attachments in the same call get disambiguated.
func planDestination(name, destDir string, usedNames map[string]int) (dst, errMsg string) {
	safeName := filepath.Base(name)
	// Within-call collision: if a previous attachment in this same message
	// already claimed this filename, bump a "-N" suffix until free. The
	// skip-if-exists check in the caller handles previous-run files
	// separately (that's pre-existing-file semantics, not dedup), so this
	// only looks at usedNames.
	safeName = uniqueFilename(safeName, usedNames)
	usedNames[strings.ToLower(safeName)]++

	dst = filepath.Join(destDir, safeName)

	// Path-traversal defense, step 2: even after filepath.Base, a Windows
	// volume-rooted name like "C:foo" can survive Join in ways that escape
	// destDir. Verify dst is inside destDir via filepath.Rel; a result
	// starting with ".." means containment was broken.
	rel, relErr := filepath.Rel(destDir, dst)
	if relErr != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Sprintf("attachment name %q would escape destination dir", name)
	}
	return dst, ""
}

// streamToDest writes the body of url to a unique temp file in destDir and
// then commits it to dst. The download always lands in a hidden .part temp
// first, so:
//   - a reader never sees a half-written or 0-byte file at dst, and
//   - a failed/aborted download leaves at most an orphan temp, never a
//     partial final file.
//
// The commit step differs by mode:
//   - overwrite=true: atomicRename replaces any existing dst.
//   - no-overwrite: os.Link the completed temp to dst. The link only appears
//     once it points at the fully-downloaded file (no placeholder window),
//     and it fails with fs.ErrExist if dst already exists → keep it (Skipped).
//     This makes the "keep existing files" guarantee race-free without ever
//     exposing dst before it holds complete content: a concurrent caller
//     either wins the link or sees a finished file, never an empty reservation.
//     (os.Link needs dst and temp on the same filesystem, which holds since
//     both live in destDir; it is unsupported on a few filesystems like FAT.)
//
// `out` is the already-populated result entry — we layer Bytes / Error
// onto it and return.
func streamToDest(ctx context.Context, cli *client.Client, url, destDir, dst string, overwrite bool, out DownloadedFile) DownloadedFile {
	// Fast path: skip an already-present file without downloading it. This is
	// purely an optimization — correctness for the race where dst appears
	// mid-download is still enforced by the os.Link commit below, which fails
	// with ErrExist rather than clobbering.
	if !overwrite {
		if info, err := os.Stat(dst); err == nil {
			out.Skipped = true
			out.Bytes = info.Size()
			return out
		}
	}

	tmpf, err := os.CreateTemp(destDir, ".edookit-download-*.part")
	if err != nil {
		out.Error = fmt.Sprintf("create temp in %s: %v", destDir, err)
		return out
	}
	tmpPath := tmpf.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmpf.Chmod(0o600); err != nil {
		_ = tmpf.Close()
		out.Error = fmt.Sprintf("chmod %s: %v", tmpPath, err)
		return out
	}

	n, copyErr := cli.GetTo(ctx, url, tmpf)
	closeErr := tmpf.Close()
	if copyErr != nil {
		out.Error = fmt.Sprintf("download: %v", copyErr)
		return out
	}
	if closeErr != nil {
		out.Error = fmt.Sprintf("close: %v", closeErr)
		return out
	}

	if !overwrite {
		// Hard-link the finished temp into place. dst becomes visible only as
		// a complete file; an existing dst makes Link fail with ErrExist so we
		// keep it. The temp name is dropped by the deferred cleanup either way.
		if err := os.Link(tmpPath, dst); err != nil {
			if errors.Is(err, fs.ErrExist) {
				out.Skipped = true
				if info, statErr := os.Stat(dst); statErr == nil {
					out.Bytes = info.Size()
				}
				return out
			}
			out.Error = fmt.Sprintf("link %s -> %s: %v", tmpPath, dst, err)
			return out
		}
		out.Bytes = n
		log.Printf("[tools] downloaded %d bytes -> %s", n, dst)
		return out
	}

	// overwrite=true: replace any existing dst atomically.
	if err := atomicRename(tmpPath, dst); err != nil {
		out.Error = fmt.Sprintf("rename %s -> %s: %v", tmpPath, dst, err)
		return out
	}
	tmpPath = "" // rename consumed the temp; deferred cleanup must not remove dst
	out.Bytes = n
	log.Printf("[tools] downloaded %d bytes -> %s", n, dst)
	return out
}

// uniqueFilename returns base if not already claimed within this call, or
// "<stem>-N.<ext>" with the smallest N >= 2 that is free. Operates on the
// usedNames map only — pre-existing files on disk are handled by the
// skip-if-exists / overwrite policy in downloadOne, not here.
//
// Keys in usedNames are normalized to lowercase so "Report.pdf" and
// "report.pdf" collide as they would on case-insensitive filesystems
// (macOS APFS default, Windows NTFS). Doing it unconditionally is harmless
// on case-sensitive ones (Linux ext4) — at worst it picks "-2" when the
// strict semantics would have allowed both names to coexist, which is a
// negligible loss of distinguishability vs. the silent corruption it
// prevents on case-insensitive volumes.
func uniqueFilename(base string, usedNames map[string]int) string {
	if _, taken := usedNames[strings.ToLower(base)]; !taken {
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		if _, taken := usedNames[strings.ToLower(candidate)]; !taken {
			return candidate
		}
	}
}

// resolveDestDir applies the default-path policy and expands a leading
// tilde. Default (when raw is empty): <os.TempDir>/edookit-mcp/<message-id>.
// os.TempDir returns the OS-appropriate temp directory (/tmp on Unix,
// %TMP%\... on Windows, $TMPDIR on macOS) so the default is portable and
// gets garbage-collected by the OS — users who want persistence pass an
// explicit path.
//
// Relative paths are rejected: the MCP server's cwd is whatever started
// the host application (Claude Desktop, Code, ChatGPT, …) which is
// unpredictable and would land files in a surprising place. The README
// contract documents this — caller must pass an absolute path or one
// starting with "~/".
func resolveDestDir(raw, msgID string) (string, error) {
	if raw == "" {
		return filepath.Join(os.TempDir(), "edookit-mcp", msgID), nil
	}
	if strings.HasPrefix(raw, "~/") || raw == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~ in %q: %w", raw, err)
		}
		if raw == "~" {
			return home, nil
		}
		return filepath.Join(home, raw[2:]), nil
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("destination_dir %q must be absolute or start with ~/ (the MCP server's cwd is not a stable anchor)", raw)
	}
	return raw, nil
}

// windowsReservedBasenames is the set of DOS device names Windows refuses
// to create as regular files, applied case-insensitively and regardless
// of extension (CON.txt is reserved too). Sourced from
// https://learn.microsoft.com/windows/win32/fileio/naming-a-file.
var windowsReservedBasenames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// windowsUnsafeName returns a non-empty reason string when name would
// misbehave on Windows. Catches:
//
//   - Reserved characters: ":" (creates an NTFS Alternate Data Stream
//     inside destDir instead of a normal file), plus < > " | ? * and
//     control bytes (0-31).
//   - Reserved DOS device basenames: CON, PRN, AUX, NUL, COM0-9, LPT0-9
//     — case-insensitive, with or without an extension.
//   - Names ending in space or '.' (Windows silently strips them and
//     creates a file under a name the user can't reliably re-open).
//
// All checks per https://learn.microsoft.com/windows/win32/fileio/naming-a-file.
// Always returns "" on non-Windows GOOS so otherwise-valid Unix filenames
// (containing "abc:def" or named "CON") aren't blocked.
func windowsUnsafeName(name string) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	for _, r := range name {
		if r < 32 {
			return "contains a control character (Windows-reserved)"
		}
		switch r {
		case ':':
			return "contains ':' (would create an NTFS Alternate Data Stream on Windows)"
		case '<', '>', '"', '|', '?', '*':
			return fmt.Sprintf("contains Windows-reserved character %q", r)
		}
	}
	// Trailing space or dot: Windows quietly strips them, so the actual
	// on-disk name doesn't match what was requested and re-opening by
	// the requested name fails.
	if last := name[len(name)-1]; last == ' ' || last == '.' {
		return "ends with space or '.' (Windows silently strips trailing space/dot)"
	}
	// DOS device basename: reserved regardless of extension. Strip
	// extension and compare uppercase against the well-known list.
	stem := name
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		stem = name[:dot]
	}
	if windowsReservedBasenames[strings.ToUpper(stem)] {
		return fmt.Sprintf("basename %q is a reserved Windows device name", stem)
	}
	return ""
}

// atomicRename os.Renames tmp to dst with one fallback: on older Windows
// or restrictive ACL configs, Rename can return fs.ErrExist when dst
// already exists (overwrite=true case) — even though Go 1.5+ MoveFileEx
// is supposed to replace it. In that case only, remove dst and retry.
// Other errors (perms, I/O, cross-device, disk full) propagate without
// touching dst so the user's previous file is preserved. Same pattern
// internal/client/cookie_store.go uses.
func atomicRename(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	if removeErr := os.Remove(dst); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		return fmt.Errorf("remove existing dst before rename retry: %w (original rename error: %w)", removeErr, err)
	}
	return os.Rename(tmp, dst)
}
