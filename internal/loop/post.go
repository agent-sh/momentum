package loop

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Reports, summaries and findings pass the secret scrubber before display or storage.
// Neutral also makes mentions and external references inert.

// Scrubber is the secret scrubber the loop redacts with: scrump's text profile, reading stdin and writing stdout.
var Scrubber = []string{"scrump", "--profile", "text", "scrub", "-", "-o", "-"}

// ErrNoScrubber means the scrubber cannot run: the loop posts and shows nothing it could not scrub.
var ErrNoScrubber = errors.New("the secret scrubber (scrump) does not run here")

// Redact is a text with every secret the scrubber finds masked.
func Redact(ctx context.Context, text string) (string, error) {
	if len(text) > MaxStream {
		return "", fmt.Errorf("text exceeds the whole-stream limit")
	}
	if text == "" {
		return "", nil
	}
	if len(Scrubber) == 0 || Scrubber[0] == "" {
		return "", ErrNoScrubber
	}
	if _, err := exec.LookPath(Scrubber[0]); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoScrubber, err)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, Scrubber[0], Scrubber[1:]...)
	cmd.Stdin = strings.NewReader(text)
	out, errOut := Capture{Limit: MaxStream}, Capture{Limit: MaxStream}
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoScrubber, err)
	}
	if out.Over() || errOut.Over() {
		return "", fmt.Errorf("%w: output exceeds the whole-stream limit", ErrNoScrubber)
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
	urlRefRe   = regexp.MustCompile(`(?i)(?:https?://|//)(?:www\.)?github\.com/[^\s<>"()]+`)
	refRe      = regexp.MustCompile(`(?i)(^|[^\w/&])((?:[\w.-]+/[\w.-]+)?#|gh-)(\d+)`)
	homePathRe = regexp.MustCompile(`(?i)(?:/(?:home|users)/|/root(?:/|\b)|~[/\\]|[a-z]:\\users\\)[^\s<>"'` + "`" + `)]*`)
	addressRe  = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9.-]+`)
	hostNameRe = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9][a-z0-9-]*)+\b`)
	ipv6Re     = regexp.MustCompile(`(?i)(?:[a-f0-9]{1,4}:){2,}[a-f0-9:]+(?:%[a-z0-9_-]+)?|::[a-f0-9:]+`)
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

// Public masks personal addresses and paths conservatively, without identifying their owner.
// Dotted names may be file names as well as hosts; both are withheld.
func Public(ctx context.Context, text string) (string, error) {
	r, err := Redact(ctx, text)
	if err != nil {
		return "", err
	}
	r = homePathRe.ReplaceAllString(r, "[personal]")
	r = addressRe.ReplaceAllString(r, "[personal]")
	r = ipv6Re.ReplaceAllString(r, "[personal]")
	r = hostNameRe.ReplaceAllString(r, "[personal]")
	return Neutral(r), nil
}
