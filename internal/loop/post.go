package loop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// What the loop writes where others read it (a commit message, a pull request's title and body, a review it
// posts, a file it shows a run) is the model's text, and a model can put anything in it: a secret it read, a
// mention that pages someone, a reference that writes into another repository's issue. Public passes it through
// the machine's secret scrubber and neutralizes mentions and references; with no scrubber, nothing is posted.

// Scrubber is the secret scrubber the loop redacts with: scrump's text profile, reading stdin and writing stdout.
var Scrubber = []string{"scrump", "--profile", "text", "scrub", "-", "-o", "-"}

// ErrNoScrubber means the scrubber cannot run: the loop posts and shows nothing it could not scrub.
var ErrNoScrubber = errors.New("the secret scrubber (scrump) does not run here")

// Redact is a text with every secret the scrubber finds masked.
func Redact(ctx context.Context, text string) (string, error) {
	if text == "" {
		return "", nil
	}
	if _, err := exec.LookPath(Scrubber[0]); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoScrubber, err)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, Scrubber[0], Scrubber[1:]...)
	cmd.Stdin = strings.NewReader(text)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoScrubber, err)
	}
	if out.Len() == 0 && strings.TrimSpace(text) != "" {
		// a scrubber that read a text and printed nothing did not scrub it: nothing goes on unscrubbed or empty
		return "", fmt.Errorf("%w: it printed nothing for a text of %d bytes", ErrNoScrubber, len(text))
	}
	return out.String(), nil
}

var (
	// mentionRe is a GitHub mention: an @ that opens a word, not one inside an address.
	mentionRe = regexp.MustCompile(`(^|[^\w.@/-])@([A-Za-z0-9][\w-]*)`)
	// refRe is a reference GitHub links and writes back into the issue it names: owner/repo#N, #N, GH-N.
	urlRefRe = regexp.MustCompile(`(?i)(?:https?://|//)(?:www\.)?github\.com/[^\s<>"()]+`)
	refRe    = regexp.MustCompile(`(?i)(^|[^\w/&])((?:[\w.-]+/[\w.-]+)?#|gh-)(\d+)`)
)

// Neutral is a text in which no mention pages anyone and no reference writes into another issue: a zero-width
// space after each @ and #, which GitHub renders unseen and does not read as a mention or a link.
func Neutral(text string) string {
	// Break the host of GitHub URLs, including encoded paths and Markdown destinations.
	text = urlRefRe.ReplaceAllStringFunc(text, func(url string) string {
		i := strings.Index(strings.ToLower(url), "github.com")
		return url[:i] + "github[.]com" + url[i+len("github.com"):]
	})
	text = mentionRe.ReplaceAllString(text, "$1@\u200b$2")
	return refRe.ReplaceAllString(text, "$1$2\u200b$3")
}

// Public is a text the loop may post or show: scrubbed of secrets, its mentions and references neutral.
func Public(ctx context.Context, text string) (string, error) {
	r, err := Redact(ctx, text)
	if err != nil {
		return "", err
	}
	return Neutral(r), nil
}

// CredentialDirs are the directories that hold credentials, under a home or anywhere in a tree.
var CredentialDirs = []string{".ssh", ".gnupg", ".aws", ".azure", ".config/gcloud", ".config/gh", ".config/glab-cli", ".config/github-copilot",
	".local/share/keyrings", ".password-store", ".claude/.credentials.json", ".claude.json", ".netrc", ".git-credentials", ".docker",
	".config/claude-code", ".config/rclone", ".npmrc", ".cargo/credentials", ".cargo/credentials.toml", ".codex/auth.json", ".kube",
	".mozilla", ".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".pki", ".local/share/momentum"}

// credentialNames are the names of files that hold a secret: keys and key stores, environment and auth files, and
// the data files whose name says they hold a token, a secret, a credential, a password or an API key. A source file
// whose name holds such a word (tokens.go, secret_store.py) is not one: its change is code, and is read.
var credentialNames = func() []string {
	names := []string{".env", ".env.*", "*.env", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.kdbx", "*.ppk",
		"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*", ".netrc", ".git-credentials", ".npmrc", ".pypirc", "auth.json", "client_secret*.json"}
	for _, word := range []string{"token", "secret", "credential", "password", "passwd", "apikey", "api_key", "api-key"} {
		names = append(names, word, word+"s", "*."+word, "*."+word+"s")
		for _, ext := range []string{"json", "yaml", "yml", "toml", "ini", "txt", "env", "conf", "cfg", "properties", "xml"} {
			names = append(names, "*"+word+"*."+ext)
		}
	}
	return names
}()

// CredentialName says whether a file's name says it holds a secret.
func CredentialName(name string) bool {
	name = strings.ToLower(name)
	for _, pattern := range credentialNames {
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// CredentialPath says whether a slash-separated path is a credential file or lies where credentials are kept,
// anywhere in a tree.
func CredentialPath(p string) bool {
	clean := path.Clean("/" + strings.ToLower(filepath.ToSlash(p)))
	for _, d := range CredentialDirs {
		d = "/" + strings.ToLower(d)
		if clean == d || strings.HasPrefix(clean, d+"/") || strings.Contains(clean, d+"/") || strings.HasSuffix(clean, d) {
			return true
		}
	}
	for _, part := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if CredentialName(part) {
			return true
		}
	}
	return false
}

// CredentialPathspecs are git pathspecs that leave the credential files and directories out of a diff, a history
// or a search, wherever they are in the tree.
func CredentialPathspecs() []string {
	var out []string
	for _, name := range credentialGlobs() {
		out = append(out, ":(top,exclude,icase,glob)**/"+name, ":(top,exclude,icase,glob)**/"+name+"/**")
	}
	return out
}

// credentialIncludes are the same pathspecs that name the credential files and directories, to find them.
func credentialIncludes() []string {
	var out []string
	for _, name := range credentialGlobs() {
		out = append(out, ":(top,icase,glob)**/"+name, ":(top,icase,glob)**/"+name+"/**")
	}
	return out
}

func credentialGlobs() []string {
	return append(append([]string{}, credentialNames...), CredentialDirs...)
}

// CredentialDenials are the file tools' rules that keep a run from reading a credential file or a directory where
// credentials are kept, in any of its directories: the same names and directories, but the store's own directory,
// under which a run's directory and the loop's work trees lie.
func CredentialDenials() []string {
	var out []string
	for _, name := range credentialNames {
		out = append(out, "Read(**/"+name+")")
	}
	for _, d := range CredentialDirs {
		if d == ".local/share/momentum" {
			continue
		}
		out = append(out, "Read(**/"+d+")", "Read(**/"+d+"/**)")
	}
	return out
}
