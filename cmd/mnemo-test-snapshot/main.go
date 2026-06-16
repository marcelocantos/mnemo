// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// mnemo-test-snapshot creates a safe, isolated copy of the user's mnemo
// data (~/.mnemo and ~/.claude/projects) for Tier 3 system tests. The
// destination is a fresh directory outside any git working tree; on
// macOS the copy uses APFS clone-by-reference (cp -c -R) so it is
// effectively free.
//
// The last line printed on stdout is `MNEMO_HOME=<dest>`; tests can
// either eval that, parse it, or just read the directory path.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// forceFallback lets tests exercise the recursive-copy path on darwin.
// It is also flipped automatically when GOOS is not darwin.
var forceFallback = false

const usage = `usage: mnemo-test-snapshot [--dest <path>] [--force] [--source-home <path>]

Creates an isolated copy of ~/.mnemo and ~/.claude/projects for Tier 3
system tests. The destination must NOT be inside a git working tree.

Flags:
  --dest <path>          Destination directory.
                         Default: <MNEMO_HOME or $HOME>/.mnemo-test-snapshots/snapshot-<timestamp>
  --force                Allow clobbering an existing destination.
  --source-home <path>   Override the source $HOME (defaults to the user's home directory).
  --help                 Print this message.

On success the last line of stdout is:
  MNEMO_HOME=<dest>
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("mnemo-test-snapshot", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		dest       string
		force      bool
		sourceHome string
		help       bool
	)
	flags.StringVar(&dest, "dest", "", "destination directory")
	flags.BoolVar(&force, "force", false, "allow clobbering an existing destination")
	flags.StringVar(&sourceHome, "source-home", "", "override the source $HOME")
	flags.BoolVar(&help, "help", false, "print usage")
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := flags.Parse(args); err != nil {
		return err
	}
	if help {
		fmt.Fprint(stdout, usage)
		return nil
	}

	// Resolve source $HOME.
	if sourceHome == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("snapshot: resolve home: %w", err)
		}
		sourceHome = h
	}

	// Pick destination. Precedence: --dest > $MNEMO_HOME > $HOME.
	if dest == "" {
		base := os.Getenv("MNEMO_HOME")
		if base == "" {
			base = sourceHome
		}
		ts := time.Now().UTC().Format("20060102T150405Z")
		dest = filepath.Join(base, ".mnemo-test-snapshots", "snapshot-"+ts)
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("snapshot: resolve dest: %w", err)
	}
	dest = absDest

	// Refuse to write into a git working tree. We check the destination's
	// parent (it may not exist yet, but its parent should). Walk upward
	// until we find an existing ancestor, then ask git about it.
	if err := refuseIfGitTracked(dest, stderr); err != nil {
		return err
	}

	// Refuse to clobber an existing destination without --force.
	if _, err := os.Stat(dest); err == nil {
		if !force {
			return fmt.Errorf("snapshot: destination %s already exists (use --force to clobber)", dest)
		}
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("snapshot: remove existing dest: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("snapshot: stat dest: %w", err)
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("snapshot: mkdir dest: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, ".claude"), 0o755); err != nil {
		return fmt.Errorf("snapshot: mkdir .claude: %w", err)
	}

	// Clone sources. Missing sources are tolerated (logged); they're not
	// fatal because a fresh user environment may legitimately lack one.
	srcMnemo := filepath.Join(sourceHome, ".mnemo")
	srcProjects := filepath.Join(sourceHome, ".claude", "projects")
	dstMnemo := filepath.Join(dest, ".mnemo")
	dstProjects := filepath.Join(dest, ".claude", "projects")

	useFallback := forceFallback || runtime.GOOS != "darwin"
	if useFallback && runtime.GOOS != "darwin" {
		fmt.Fprintln(stderr, "[fallback] non-darwin platform: using recursive copy")
	} else if useFallback {
		fmt.Fprintln(stderr, "[fallback] forced: using recursive copy")
	}

	for _, p := range []struct{ src, dst string }{
		{srcMnemo, dstMnemo},
		{srcProjects, dstProjects},
	} {
		if _, err := os.Stat(p.src); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "[skip] source %s does not exist\n", p.src)
			if err := os.MkdirAll(p.dst, 0o755); err != nil {
				return fmt.Errorf("snapshot: mkdir %s: %w", p.dst, err)
			}
			continue
		} else if err != nil {
			return fmt.Errorf("snapshot: stat %s: %w", p.src, err)
		}
		if useFallback {
			if err := copyTree(p.src, p.dst); err != nil {
				return fmt.Errorf("snapshot: copy %s -> %s: %w", p.src, p.dst, err)
			}
		} else {
			// cp -c (clonefile) -R (recursive) -p (preserve). The parent of
			// p.dst already exists; cp will create p.dst itself.
			cmd := exec.Command("cp", "-c", "-R", p.src, p.dst)
			cmd.Stdout = stderr
			cmd.Stderr = stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("snapshot: cp -c -R %s %s: %w", p.src, p.dst, err)
			}
		}
	}

	// Ensure a config.json exists; mnemo defaults handle the rest.
	cfgPath := filepath.Join(dstMnemo, "config.json")
	if _, err := os.Stat(cfgPath); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(cfgPath, []byte("{}\n"), 0o644); err != nil {
			return fmt.Errorf("snapshot: write stub config: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("snapshot: stat config: %w", err)
	}

	// LAST line on stdout: tests rely on this.
	fmt.Fprintf(stdout, "MNEMO_HOME=%s\n", dest)
	return nil
}

// refuseIfGitTracked walks upward from dest looking for an existing
// ancestor and asks git whether it is inside a working tree. If so,
// abort with a clear message.
func refuseIfGitTracked(dest string, stderr io.Writer) error {
	probe := dest
	for {
		if info, err := os.Stat(probe); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// Hit the filesystem root without finding an existing dir.
			return nil
		}
		probe = parent
	}
	cmd := exec.Command("git", "-C", probe, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		// Not a git repo (or git is missing): fine, proceed.
		return nil
	}
	toplevel := strings.TrimSpace(string(out))
	return fmt.Errorf(
		"snapshot: destination %s is inside the git working tree at %s; "+
			"point --dest at a non-tracked location (conventional: "+
			"$HOME/.mnemo-test-snapshots/snapshot-<timestamp>)",
		dest, toplevel,
	)
}

// copyTree does a plain recursive copy of src into dst, creating dst
// itself. Symlinks are recreated as symlinks; permissions are preserved.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// Best-effort: remove existing target so Symlink doesn't fail.
			_ = os.Remove(target)
			return os.Symlink(link, target)
		default:
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
	})
}
