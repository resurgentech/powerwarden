package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultBaoMount is OpenBao's generic default kv mount.
const DefaultBaoMount = "kv"

// ErrNoSuchName means OpenBao answered but holds no token under that name.
var ErrNoSuchName = errors.New("no token under that name")

// FetchToken runs `bao kv get -mount=<mount> -field=NAME <path>` with the
// caller's own environment and OpenBao login. It has no credentials of its own.
func FetchToken(ctx context.Context, mount, path, name string) (string, error) {
	cmd := exec.CommandContext(ctx, "bao", "kv", "get", "-mount="+mount, "-field="+name, path)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		var nf *exec.Error
		if errors.As(err, &nf) {
			return "", fmt.Errorf("bao not found on PATH: install OpenBao's bao CLI and log in")
		}
		if strings.Contains(msg, "not present") || strings.Contains(msg, "No value found") {
			return "", ErrNoSuchName
		}
		return "", fmt.Errorf("bao kv get failed (is BAO_ADDR set and are you logged in with `bao login`?): %v: %s", err, msg)
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" {
		return "", ErrNoSuchName
	}
	return tok, nil
}

// ErrExists means the token file exists and force was not given.
var ErrExists = errors.New("token file exists")

// WriteToken writes the token to path atomically: the directory is created
// 0700, the content goes to a 0600 temp file in the same directory, then it is
// renamed (force) or hard-linked (no force, so an existing file is never
// replaced, even by a racing writer) into place.
func WriteToken(path, token string, force bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !force {
		if _, err := os.Lstat(path); err == nil {
			return ErrExists
		}
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if force {
		return os.Rename(tmp.Name(), path)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return err
	}
	return nil
}
