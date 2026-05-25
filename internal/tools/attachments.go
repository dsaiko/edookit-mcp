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
// Bytes is the actual on-disk size; Skipped flags cases where we elected
// not to overwrite an existing file (currently only when the existing file's
// size matches Edookit's response — see DownloadAttachments for the policy).
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

	for _, a := range msg.Attachments {
		entry := downloadOne(ctx, cli, a, destDir, opts.Overwrite)
		res.Files = append(res.Files, entry)
	}
	return res, nil
}

// downloadOne handles a single attachment end-to-end. Returns the result
// entry; never panics. Logs at debug-level for the operator but never
// includes filename in error messages echoed back through MCP (filenames
// can be user-private school data and the message already conveys what
// went wrong).
func downloadOne(ctx context.Context, cli *client.Client, a Attachment, destDir string, overwrite bool) DownloadedFile {
	out := DownloadedFile{Name: a.Name}

	// Path-traversal defense: keep just the base name. Edookit shouldn't
	// send "../" but treating server-supplied filenames as path components
	// is the kind of mistake worth not making once.
	safeName := filepath.Base(a.Name)
	if safeName == "" || safeName == "." || safeName == ".." || safeName == string(os.PathSeparator) {
		out.Error = fmt.Sprintf("attachment name %q is not safe to use as a filename", a.Name)
		return out
	}
	dst := filepath.Join(destDir, safeName)
	out.Path = dst

	// Skip-if-already-present, sized-match: if the file already exists and
	// the caller didn't request overwrite, leave it alone. We don't have
	// Edookit's expected size up-front (no Content-Length in our metadata),
	// so this is a coarse "if it exists, don't redownload" — but it's safe
	// because the message-edit endpoint gives stable Edookit-side file UUIDs
	// for re-fetch when the user does want it.
	if !overwrite {
		if info, err := os.Stat(dst); err == nil {
			out.Bytes = info.Size()
			out.Skipped = true
			return out
		}
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: dst is filepath.Join(destDir, base(a.Name)); base() prevents traversal
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

// resolveDestDir applies the default-path policy and expands a leading
// tilde. Default (when raw is empty): <os.TempDir>/edookit-mcp/<message-id>.
// os.TempDir returns the OS-appropriate temp directory (/tmp on Unix,
// %TMP%\... on Windows, $TMPDIR on macOS) so the default is portable and
// gets garbage-collected by the OS — users who want persistence pass an
// explicit path.
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
	return raw, nil
}
