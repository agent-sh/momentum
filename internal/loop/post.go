package loop

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	ipv4Re     = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	// Take the whole token so an identifier path cannot yield a hex suffix.
	ipv6Re        = regexp.MustCompile(`(?i)[a-z0-9_]*:[a-z0-9_:]+(?:%[a-z0-9_-]+)?`)
	hostContextRe = regexp.MustCompile(`(?i)(?:\bhost:\s*|\bssh\s+)$`)
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

// Public masks personal addresses, paths and domain-shaped hosts without identifying their owner.
func Public(ctx context.Context, text string) (string, error) {
	r, err := Redact(ctx, text)
	if err != nil {
		return "", err
	}
	return Neutral(maskPersonal(r)), nil
}

// Source names and versions are dotted too. A short domain suffix list keeps
// main.go, notes.txt, config.json and go1.22.3 useful in review locations.
var hostSuffixes = map[string]bool{
	"com": true, "org": true, "net": true, "edu": true, "gov": true, "mil": true,
	"io": true, "ai": true, "app": true, "dev": true, "co": true, "uk": true,
	"example": true, "invalid": true, "test": true, "local": true, "internal": true,
}

func maskPersonal(text string) string {
	text = homePathRe.ReplaceAllString(text, "[personal]")
	text = addressRe.ReplaceAllString(text, "[personal]")
	text = maskIPv6(text)
	text = ipv4Re.ReplaceAllString(text, "[personal]")
	matches := hostNameRe.FindAllStringIndex(text, -1)
	var out strings.Builder
	end := 0
	for _, span := range matches {
		host := text[span[0]:span[1]]
		last := strings.LastIndexByte(host, '.')
		prefix := text[:span[0]]
		if !hostSuffixes[strings.ToLower(host[last+1:])] {
			continue
		}
		explicit := strings.HasSuffix(prefix, "://") || strings.HasSuffix(prefix, "@") || hostContextRe.MatchString(prefix)
		suffix := text[span[1]:]
		labels := strings.Split(host, ".")
		// Bare receiver paths and calls remain code. A URL or host command
		// establishes address context even when its host resembles a receiver.
		receiver := len(labels[0]) <= 3 && identifierLabel(labels[0])
		switch strings.ToLower(labels[0]) {
		case "obj", "self", "this", "cfg", "config":
			receiver = true
		}
		beforePath := span[0] >= 2 && text[span[0]-1] == '.' && identifierByte(text[span[0]-2])
		// The regexp may start inside a receiver containing an underscore.
		insideIdentifier := span[0] > 0 && identifierByte(text[span[0]-1])
		hostLike := len(labels) >= 3 && host == strings.ToLower(host) && !receiver && !beforePath && !insideIdentifier &&
			!strings.HasPrefix(suffix, "(")
		if !explicit && !hostLike && !strings.HasPrefix(suffix, "/") {
			continue
		}
		out.WriteString(text[end:span[0]])
		out.WriteString("[personal]")
		end = span[1]
	}
	out.WriteString(text[end:])
	return out.String()
}

func maskIPv6(text string) string {
	var out strings.Builder
	end := 0
	for _, span := range ipv6Re.FindAllStringIndex(text, -1) {
		token := text[span[0]:span[1]]
		address, _, _ := strings.Cut(token, "%")
		start := span[0]
		if label, rest, ok := strings.Cut(address, ":"); ok && label != "" && !hexLabel(label) {
			start += len(label) + 1
			address = rest
		}
		groups := 0
		lowerLettersOnly := true
		for _, group := range strings.Split(address, ":") {
			if group != "" {
				groups++
			}
			for i := range group {
				if group[i] < 'a' || group[i] > 'f' {
					lowerLettersOnly = false
				}
			}
		}
		// Two lowercase hex-letter identifiers also parse as compressed IPs.
		// Digits, uppercase hex or a third group establish address context.
		if groups < 2 || (groups < 3 && lowerLettersOnly) || net.ParseIP(address) == nil {
			continue
		}
		// A dotted receiver or a call makes this an identifier path.
		beforePath := span[0] >= 2 && text[span[0]-1] == '.' && identifierByte(text[span[0]-2])
		afterPath := span[1]+1 < len(text) && text[span[1]] == '.' && identifierByte(text[span[1]+1])
		if beforePath || afterPath || strings.HasPrefix(text[span[1]:], "(") {
			continue
		}
		out.WriteString(text[end:start])
		out.WriteString("[personal]")
		end = span[1]
	}
	out.WriteString(text[end:])
	return out.String()
}

func hexLabel(label string) bool {
	for i := range label {
		c := label[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func identifierLabel(label string) bool {
	if label == "" || !identifierByte(label[0]) || label[0] >= '0' && label[0] <= '9' {
		return false
	}
	for i := range label {
		if !identifierByte(label[i]) {
			return false
		}
	}
	return true
}

func identifierByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
