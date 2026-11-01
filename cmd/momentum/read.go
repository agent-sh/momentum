package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// A loop run's file tools reach only its own directories; it reads the rest of the machine through these
// commands, which print nothing the secret scrubber has not passed: a record an event names, a file, a service's
// state and log. Each refuses where credentials are kept.

// readMax is the most of a record or a file these commands print.
const readMax = 256 << 10

// scrubbed writes a text through the secret scrubber, or refuses to write it.
func scrubbed(ctx context.Context, w io.Writer, text string) error {
	out, err := loop.Redact(ctx, text)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, out)
	return err
}

// evidenceShow prints an event of the store and, where its reference names a record of a transcript or a log
// (path@offset), that record as the file holds it, scrubbed: what the event's summary cut. Only an event the store
// holds is shown, so a store made as of a moment shows nothing after it.
func evidenceShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usageError{"evidence show takes one event: its id, or ev:ID"}
	}
	id := strings.TrimPrefix(args[0], "ev:")
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	e, ok, err := st.EventByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return usageError{fmt.Sprintf("the store holds no event %s", id)}
	}
	fmt.Fprintf(stdout, "%s %s %s %s %s %s\n  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.Source, e.Actor, e.Kind, e.Session,
		plain.Line(e.Tool), plain.Line(e.Summary))
	path, off, ok := placeOf(e.Ref)
	if !ok {
		return nil
	}
	if why := secretPath(path); why != "" {
		fmt.Fprintf(stdout, "  (its record is not shown: %s)\n", why)
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stdout, "  (its record is gone: %v)\n", err)
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	line, err := bufio.NewReaderSize(f, 1<<16).ReadSlice('\n')
	if err != nil && err != bufio.ErrBufferFull && len(line) == 0 {
		fmt.Fprintf(stdout, "  (its record cannot be read: %v)\n", err)
		return nil
	}
	if len(line) > readMax {
		line = line[:readMax]
	}
	return scrubbed(ctx, stdout, strings.ToValidUTF8(strings.TrimRight(string(line), "\n"), "?")+"\n")
}

// placeOf splits a reference (path@offset, with #index) into its file and the offset of its record.
func placeOf(ref string) (string, int64, bool) {
	at := strings.LastIndexByte(ref, '@')
	if at <= 0 {
		return "", 0, false
	}
	path, rest := ref[:at], ref[at+1:]
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	off, err := strconv.ParseInt(rest, 10, 64)
	return path, off, err == nil && strings.HasPrefix(path, "/")
}

// secretDirs are where the user's credentials are kept, under his home: none of it is shown.
var secretDirs = []string{".ssh", ".gnupg", ".aws", ".azure", ".config/gcloud", ".config/gh", ".config/glab-cli", ".config/github-copilot",
	".local/share/keyrings", ".password-store", ".claude/.credentials.json", ".claude.json", ".netrc", ".git-credentials", ".docker",
	".config/claude-code", ".config/rclone", ".npmrc", ".cargo/credentials", ".cargo/credentials.toml", ".codex/auth.json", ".kube",
	".mozilla", ".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".pki", ".local/share/momentum"}

// secretName is a file's name that says it holds a secret.
var secretName = regexp.MustCompile(`(?i)(^\.env($|\.)|\.pem$|\.key$|\.p12$|\.pfx$|^id_(rsa|dsa|ecdsa|ed25519)|credential|secret|token|\.kdbx$|\.keystore$)`)

// secretPath says why a path is not shown, or "".
func secretPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return err.Error()
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	home := paths.Home()
	for _, d := range secretDirs {
		if within(abs, filepath.Join(home, d)) {
			return "it is where credentials are kept"
		}
	}
	if secretName.MatchString(filepath.Base(abs)) {
		return "its name says it holds a secret"
	}
	return ""
}

// evidenceFile prints a file of the machine, scrubbed: all of it up to 256 KiB, or its last lines. A file where
// credentials are kept, or named as one that holds them, is refused.
func evidenceFile(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence file", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tail := fs.Int("tail", 0, "its last N lines only")
	var file string
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && file == "" {
			file = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if file == "" || fs.NArg() > 0 {
		return usageError{"evidence file takes one file, and --tail N"}
	}
	file = paths.Expand(file)
	if why := secretPath(file); why != "" {
		return usageError{fmt.Sprintf("%s is not shown: %s", file, why)}
	}
	st, err := os.Stat(file)
	if err != nil {
		return usageError{err.Error()}
	}
	if !st.Mode().IsRegular() {
		return usageError{fmt.Sprintf("%s is not a file", file)}
	}
	f, err := os.Open(file)
	if err != nil {
		return usageError{err.Error()}
	}
	defer f.Close()
	if *tail > 0 && st.Size() > readMax {
		f.Seek(st.Size()-readMax, io.SeekStart)
	}
	b, err := io.ReadAll(io.LimitReader(f, readMax))
	if err != nil {
		return err
	}
	text := strings.ToValidUTF8(string(b), "?")
	if *tail > 0 {
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		if len(lines) > *tail {
			lines = lines[len(lines)-*tail:]
		}
		text = strings.Join(lines, "\n") + "\n"
	} else if st.Size() > readMax {
		text += fmt.Sprintf("\n(cut at %d of its %d bytes; --tail N reads its end)\n", readMax, st.Size())
	}
	return scrubbed(ctx, stdout, text)
}

var unitRe = regexp.MustCompile(`^[A-Za-z0-9@._:-]{1,128}$`)

// evidenceUnit prints a user service's state and the end of its log, scrubbed: systemd's status, and journald's
// last lines of the unit (200, or --lines N up to 2000).
func evidenceUnit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("evidence unit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lines := fs.Int("lines", 200, "how many of its log's last lines")
	var unit string
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && unit == "" {
			unit = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if !unitRe.MatchString(unit) || fs.NArg() > 0 {
		return usageError{"evidence unit takes one user unit's name, and --lines N"}
	}
	n := min(max(*lines, 1), 2000)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, _ := exec.CommandContext(cctx, "systemctl", "--user", "status", "--no-pager", "--lines=0", "--", unit).CombinedOutput()
	log, _ := exec.CommandContext(cctx, "journalctl", "--user", "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(n), "-u", unit).CombinedOutput()
	text := string(status) + "\n" + string(log)
	if len(text) > readMax {
		text = text[len(text)-readMax:]
	}
	return scrubbed(ctx, stdout, strings.ToValidUTF8(text, "?"))
}
