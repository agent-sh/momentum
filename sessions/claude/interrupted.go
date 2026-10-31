package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// Prompted reports whether the transcript at path, from byte offset from on, records text as a prompt that opened
// a turn: a user record, not one the harness injected, whose text is text, whitespace folded. interrupted reports
// whether an interruption came before it: the end of the turn it waited on. It reads what was appended since a
// scan (from is the size the scan read), so a slow write shows on the next look.
func Prompted(path string, from int64, text string) (found, interrupted bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return false, false, err
	}
	want := strings.Join(strings.Fields(text), " ")
	rd := bufio.NewReaderSize(f, 1<<16)
	for !found {
		raw, rerr := rd.ReadBytes('\n')
		if rerr != nil { // a partial last line is not written yet
			if errors.Is(rerr, io.EOF) {
				break
			}
			return found, interrupted, rerr
		}
		var e line
		if json.Unmarshal(raw, &e) != nil || e.Type != "user" || e.IsSidechain || e.IsMeta || e.Message == nil {
			continue
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
			interrupted = true
		case strings.Join(strings.Fields(s), " ") == want:
			found = true
		}
	}
	return found, interrupted, nil
}
