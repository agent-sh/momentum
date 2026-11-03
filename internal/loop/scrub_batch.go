package loop

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ScrubBatchBytes caps the input and each output of a scrubber process at 8 MiB.
const ScrubBatchBytes = 8 << 20

// RedactAll scrubs records in order, once per bounded batch. A separator in a
// record becomes a space. No partial result is returned when any batch fails.
func RedactAll(ctx context.Context, texts []string) ([]string, error) {
	if len(Scrubber) == 0 || Scrubber[0] == "" {
		return nil, ErrNoScrubber
	}
	if _, err := exec.LookPath(Scrubber[0]); err != nil {
		return nil, fmt.Errorf("%w: executable unavailable", ErrNoScrubber)
	}
	args := append([]string(nil), Scrubber[1:]...)
	if filepath.Base(Scrubber[0]) == "scrump" {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--profile" && args[i+1] == "text" {
				args = append(args[:i+2], append([]string{"--records"}, args[i+2:]...)...)
				break
			}
		}
	}
	out := make([]string, 0, len(texts))
	for start := 0; start < len(texts); {
		var batch []string
		size := 0
		for start+len(batch) < len(texts) {
			text := strings.ReplaceAll(texts[start+len(batch)], "\x1e", " ")
			if len(text) > ScrubBatchBytes {
				return nil, fmt.Errorf("record exceeds the scrub batch limit")
			}
			add := len(text)
			if len(batch) > 0 {
				add++
			}
			if size+add > ScrubBatchBytes {
				break
			}
			batch, size = append(batch, text), size+add
		}
		input := strings.Join(batch, "\x1e")
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		cmd := exec.CommandContext(cctx, Scrubber[0], args...)
		cmd.Stdin = strings.NewReader(input)
		stdout, stderr := Capture{Limit: ScrubBatchBytes}, Capture{Limit: ScrubBatchBytes}
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%w: process failed", ErrNoScrubber)
		}
		if stdout.Over() || stderr.Over() {
			return nil, fmt.Errorf("%w: output exceeds the scrub batch limit", ErrNoScrubber)
		}
		if stdout.Len() == 0 && strings.TrimSpace(input) != "" {
			return nil, fmt.Errorf("%w: empty output", ErrNoScrubber)
		}
		records := strings.Split(stdout.String(), "\x1e")
		if len(records) != len(batch) {
			return nil, fmt.Errorf("%w: record count changed", ErrNoScrubber)
		}
		out = append(out, records...)
		start += len(batch)
	}
	return out, nil
}

// CheckScrubber checks the AWS documentation example access key id and a fake
// password assignment. Both values must disappear before any input is copied.
func CheckScrubber(ctx context.Context) error {
	key := "AKIA" + "IOSFODNN7EXAMPLE"
	marker := "marker-" + "7f3a"
	out, err := RedactAll(ctx, []string{key + "\n" + "password=" + marker + "\n"})
	if err != nil {
		return err
	}
	if strings.Contains(out[0], key) || strings.Contains(out[0], marker) {
		return fmt.Errorf("%w: canary survived", ErrNoScrubber)
	}
	return nil
}
