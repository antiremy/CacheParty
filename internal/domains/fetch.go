package domains

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxFileSize  = 16 << 20
	maxTotalSize = 128 << 20
)

// Fetch refreshes dir with the given repo/branch. GitHub repositories are
// downloaded as a tarball over client (no git binary needed, which matters
// in a distroless image); anything else falls back to the git CLI. The
// directory is replaced atomically, so a failed fetch leaves the previous
// copy untouched.
func Fetch(ctx context.Context, client *http.Client, repo, branch, dir string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".cache-domains-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if u, ok := TarballURL(repo, branch); ok {
		err = fetchTarball(ctx, client, u, tmp)
	} else {
		err = gitClone(ctx, repo, branch, tmp)
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(tmp, "cache_domains.json")); err != nil {
		return fmt.Errorf("fetched repo has no cache_domains.json")
	}
	return swapDir(tmp, dir)
}

// TarballURL maps https://github.com/owner/repo(.git) to its codeload URL.
func TarballURL(repo, branch string) (string, bool) {
	u, err := url.Parse(repo)
	if err != nil || (u.Host != "github.com" && u.Host != "www.github.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s",
		parts[0], parts[1], url.PathEscape(branch)), true
}

func fetchTarball(ctx context.Context, client *http.Client, u, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: %s", u, resp.Status)
	}
	return extractTarGz(resp.Body, dest)
}

// extractTarGz unpacks r into dest, stripping the leading directory GitHub
// adds. Only regular files and directories are created and every path must
// stay inside dest.
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		_, rel, ok := strings.Cut(path.Clean(hdr.Name), "/")
		if !ok || !filepath.IsLocal(rel) {
			continue // top-level dir itself, pax headers, or unsafe path
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > maxFileSize || total+hdr.Size > maxTotalSize {
				return fmt.Errorf("archive too large at %s", hdr.Name)
			}
			total += hdr.Size
			if err := writeFile(target, tr); err != nil {
				return err
			}
		}
	}
}

func writeFile(target string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(r, maxFileSize)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func gitClone(ctx context.Context, repo, branch, dest string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", branch, repo, dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// swapDir replaces dir with tmp, keeping the old copy until the new one is
// in place.
func swapDir(tmp, dir string) error {
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	old := dir + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.Rename(old, dir) // roll back
		return err
	}
	return os.RemoveAll(old)
}
