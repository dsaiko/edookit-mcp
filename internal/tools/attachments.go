package tools

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	Overwrite bool // if false, an existing file with the same byte count is left untouched
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

	// Server-supplied URL validation: an empty / whitespace-only link
	// would otherwise resolve through Client.resolve("") to the base URL
	// itself, so we'd happily save the Edookit landing page as the
	// "attachment". Fail per-file instead.
	if strings.TrimSpace(a.URL) == "" {
		out.Error = "attachment has no download URL"
		return out
	}

	// Path-traversal defense, step 1: keep just the base name. Edookit
	// shouldn't send "../" but treating server-supplied filenames as path
	// components is the kind of mistake worth not making once.
	safeName := filepath.Base(a.Name)
	if safeName == "" || safeName == "." || safeName == ".." || safeName == string(os.PathSeparator) {
		out.Error = fmt.Sprintf("attachment name %q is not safe to use as a filename", a.Name)
		return out
	}

	// Within-call collision: if a previous attachment in this same message
	// already claimed this filename, bump a "-N" suffix until free. The
	// skip-if-exists check below handles previous-run files separately
	// (that's pre-existing-file semantics, not dedup), so this only looks
	// at usedNames.
	safeName = uniqueFilename(safeName, usedNames)
	usedNames[safeName]++

	dst := filepath.Join(destDir, safeName)

	// Path-traversal defense, step 2: even after filepath.Base, a Windows
	// volume-rooted name like "C:foo" can survive Join in ways that escape
	// destDir. Verify dst is inside destDir via filepath.Rel; a result
	// starting with ".." means containment was broken.
	rel, relErr := filepath.Rel(destDir, dst)
	if relErr != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		out.Error = fmt.Sprintf("attachment name %q would escape destination dir", a.Name)
		return out
	}
	out.Path = dst

	// Skip-if-already-present: if the file already exists at dst and the
	// caller didn't request overwrite, leave it alone. No size check —
	// we have no Content-Length up-front; if the user wants a re-fetch
	// they pass overwrite=true.
	if !overwrite {
		if info, err := os.Stat(dst); err == nil {
			out.Bytes = info.Size()
			out.Skipped = true
			return out
		}
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: dst is filepath.Join(destDir, base(a.Name)) with a filepath.Rel containment check above
	if err != nil {
		out.Error = fmt.Sprintf("create %s: %v", dst, err)
		return out
	}

	n, copyErr := cli.GetTo(ctx, a.URL, f)
	closeErr := f.Close()
	if copyErr != nil {
		// Remove the partial file so a retry doesn't see it as "complete enough".
		_ = os.Remove(dst)
		out.Error = fmt.Sprintf("download: %v", copyErr)
		return out
	}
	if closeErr != nil {
		out.Error = fmt.Sprintf("close: %v", closeErr)
		return out
	}
	out.Bytes = n
	log.Printf("[tools] downloaded %d bytes -> %s", n, dst)
	return out
}

// uniqueFilename returns base if not already claimed within this call, or
// "<stem>-N.<ext>" with the smallest N >= 2 that is free. Operates on the
// usedNames map only — pre-existing files on disk are handled by the
// skip-if-exists / overwrite policy in downloadOne, not here.
func uniqueFilename(base string, usedNames map[string]int) string {
	if _, taken := usedNames[base]; !taken {
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		if _, taken := usedNames[candidate]; !taken {
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
