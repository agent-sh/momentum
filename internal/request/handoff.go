package request

import (
	"fmt"
	"os"
	"strings"

	"github.com/agent-sh/momentum/internal/evidenceview"
	"github.com/agent-sh/momentum/internal/plain"
)

const timeFormat = "2006-01-02 15:04"

// pasteLine is the handoff's last sentence, before its closing id line: where a pasted handoff ends (closing).
const pasteLine = "To continue it in another session, paste all of this there: its first and last lines link that session to the request."

// Handoff limits: how much of a request the text holds.
const (
	followUps   = 5   // the latest follow-ups after the opening
	artifacts   = 12  // the artifacts shown: commits, pushes and pull requests first, then files
	evidenceIDs = 12  // the events it names
	wordsChars  = 400 // an ask's words
)

// Line is the id line that opens and closes a handoff: a session it is pasted into links back by it.
func Line(id string) string { return "momentum request " + id }

// Handoff is the text to copy into another session: the request in the owner's words, its state, what was observed
// done, the agent's last report as its claim, the open step, its threads and the evidence, between two id lines.
func Handoff(rq *Request) string {
	var b strings.Builder
	line := func(format string, a ...any) { b.WriteString(plain.Line(fmt.Sprintf(format, a...)) + "\n") }
	line("%s - a handoff of the owner's request", Line(rq.ID))
	var evidence []string
	cite := func(id string) {
		if id != "" && len(evidence) < evidenceIDs {
			for _, e := range evidence {
				if e == "ev:"+id {
					return
				}
			}
			evidence = append(evidence, "ev:"+id)
		}
	}
	line("Asked (the owner's words, oldest first):")
	if rq.Opened.IsZero() {
		line("- unknown: the request's opening is not in this store; it came here by its handoff")
	}
	asks := rq.Asks
	shown := asks
	left := 0
	if len(asks) > followUps+1 {
		shown = append([]Ask{asks[0]}, asks[len(asks)-followUps:]...)
		left = len(asks) - followUps - 1
	}
	for i, a := range shown {
		words := plain.Clip(plain.Line(a.Words), wordsChars)
		switch {
		case a.Handoff != "" && words == "":
			words = "pasted the handoff of " + a.Handoff
		case a.Handoff != "":
			words += " (with the handoff of " + a.Handoff + ")"
		}
		line("- %s, %s %s: %s", a.At.Local().Format(timeFormat), a.Harness, short(a.Thread), words)
		cite(a.Event)
		if i == 0 && left > 0 {
			line("- (%d follow-ups between left out)", left)
		}
	}
	line("State: %s", stateLine(rq))
	line("Observed done (from what the tools printed when they ran):")
	if len(rq.Results) == 0 {
		line("- nothing: no commit, push, pull request or file write was observed")
	}
	// commits, pushes and pull requests before files, each newest first: files never push them out of the shown
	var listed []Result
	for _, files := range []bool{false, true} {
		for _, x := range rq.Results {
			if (x.Kind == KindFile) == files {
				listed = append(listed, x)
			}
		}
	}
	for i, x := range listed {
		if i == artifacts {
			line("- (%d more)", len(listed)-artifacts)
			break
		}
		text := x.Text
		if x.Kind == KindFile {
			text = "wrote " + x.Text
			if x.Writes > 1 {
				text += fmt.Sprintf(" (%d writes)", x.Writes)
			}
		}
		line("- %s %s", x.At.Local().Format(timeFormat), text)
		cite(x.Event)
	}
	if rq.CutGone > 0 {
		line("- details cut, transcript gone: %d of its calls the store cut or joined could not be read whole; what they made may be missing", rq.CutGone)
	}
	if rq.CutUnread > 0 {
		line("- details cut: %d of its calls the store cut or joined were not read whole here (no reader of their harness)", rq.CutUnread)
	}
	if rq.CutUnmasked > 0 {
		line("- details cut, not masked: %d of its calls the store cut or joined were read whole, but what they made could not be masked, so it is not kept", rq.CutUnmasked)
	}
	if rq.Report != nil {
		line("Claimed (the agent's last reply, %s, not verified): %s", rq.Report.At.Local().Format(timeFormat), rq.Report.Text)
		cite(rq.Report.Event)
	} else {
		line("Claimed: unknown, no reply of the agent's is recorded")
	}
	if rq.Open != "" {
		line("Open step: %s", rq.Open)
	} else {
		line("Open step: unknown, nothing records one; the last reply above says what the agent claims is left")
	}
	line("Sessions:")
	for _, th := range rq.Threads {
		phase := "phase unknown"
		if th.Phase != "" {
			phase = fmt.Sprintf("last seen %s at %s", th.Phase, th.PhaseAt.Local().Format(timeFormat))
			if !th.Trusted {
				phase += ", before the last turn in it"
			}
		}
		if !th.Current {
			phase = "since moved on to another request"
		}
		where := ""
		if th.Cwd != "" {
			where = " in " + th.Cwd
		}
		line("- %s %s%s, %s to %s, %s", th.Harness, th.ID, where, th.First.Local().Format(timeFormat), th.Last.Local().Format(timeFormat), phase)
	}
	if len(evidence) > 0 {
		// each line a command evidence show takes: at most ShowMost ids
		line("Evidence:")
		for i := 0; i < len(evidence); i += evidenceview.ShowMost {
			line("momentum evidence show %s", strings.Join(evidence[i:min(i+evidenceview.ShowMost, len(evidence))], " "))
		}
	}
	line("%s", pasteLine)
	line("%s", Line(rq.ID))
	return foldHome(b.String())
}

// foldHome writes the home directory as ~ wherever a text names it as a path's start: a handoff is pasted on this
// machine, so its working directories and files read the same, and the home's name stays out of the paste.
func foldHome(text string) string {
	home, err := os.UserHomeDir()
	home = strings.TrimRight(home, "/")
	if err != nil || home == "" {
		return text
	}
	pathByte := func(c byte) bool {
		return c == '/' || c == '.' || c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	var b strings.Builder
	for {
		i := strings.Index(text, home)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := i + len(home)
		if (i == 0 || !pathByte(text[i-1])) && (end == len(text) || text[end] == '/' || !pathByte(text[end])) {
			b.WriteString(text[:i] + "~")
		} else {
			b.WriteString(text[:end])
		}
		text = text[end:]
	}
}

// stateLine says the state and what it rests on.
func stateLine(rq *Request) string { return rq.State + " - " + rq.Why }
