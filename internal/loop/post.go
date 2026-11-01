package loop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
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
