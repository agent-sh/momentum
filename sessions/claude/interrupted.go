package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// appended calls each for every complete line of the transcript at path from byte offset from on, until it
// returns false. A partial last line is not written yet.
func appended(path string, from int64, each func(e line, raw []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(f, 1<<16)
	for {
		raw, err := rd.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var e line
		if json.Unmarshal(raw, &e) != nil || e.IsSidechain {
			continue
		}
		if !each(e, raw) {
			return nil
		}
	}
}

// Prompted reports whether the transcript at path, from byte offset from on (the size a scan read), records text
// as a prompt that opened a turn: a user record, not one the harness injected, whose text is text, whitespace
// folded. interrupted is when an interruption came before it, the end of the turn it waited behind; zero when none
// did.
func Prompted(path string, from int64, text string) (found bool, interrupted time.Time, err error) {
	want := strings.Join(strings.Fields(text), " ")
	err = appended(path, from, func(e line, _ []byte) bool {
		if e.Type != "user" || e.IsMeta || e.Message == nil {
			return true
		}
		var s string
		if json.Unmarshal(e.Message.Content, &s) != nil {
			var bs []block
			json.Unmarshal(e.Message.Content, &bs)
			for _, b := range bs {
				if b.Type == "text" {
					s += b.Text
				}
			}
		}
		s = strings.TrimSpace(reSystemTag.ReplaceAllString(s, ""))
		switch {
		case strings.HasPrefix(s, "[Request interrupted"):
			interrupted, _ = time.Parse(time.RFC3339Nano, e.Timestamp)
		case strings.Join(strings.Fields(s), " ") == want:
			found = true
			return false
		}
		return true
	})
	return found, interrupted, err
}

// Answered reports whether the transcript at path, from byte offset from on, holds a record that answers a model
// request sent at sent, as the tracker reads one: an assistant record, a user record the harness did not inject,
// or a system record other than a notice, dated at or after the request.
func Answered(path string, from int64, sent time.Time) (bool, error) {
	answered := false
	err := appended(path, from, func(e line, _ []byte) bool {
		at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		if !at.IsZero() && at.Before(sent) {
			return true
		}
		switch {
		case e.Type == "assistant", e.Type == "user" && !e.IsMeta, e.Type == "system" && e.Subtype != "informational":
			answered = true
			return false
		}
		return true
	})
	return answered, err
}
