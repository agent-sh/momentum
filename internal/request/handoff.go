package request

import (
	"fmt"
	"strings"

	"github.com/agent-sh/momentum/internal/plain"
)

const timeFormat = "2006-01-02 15:04"

// Handoff limits: how much of a request the text holds.
const (
	followUps   = 5   // the latest follow-ups after the opening
	artifacts   = 12  // the newest artifacts
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
	for i, x := range rq.Results {
		if i == artifacts {
			line("- (%d more)", len(rq.Results)-artifacts)
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
	if rq.Report != nil {
		line("Claimed (the agent's last reply, %s, not checked): %s", rq.Report.At.Local().Format(timeFormat), rq.Report.Text)
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
		line("Evidence (momentum evidence show ID): %s", strings.Join(evidence, " "))
	}
	line("To continue it in another session, paste all of this there: its first and last lines link that session to the request.")
	line("%s", Line(rq.ID))
	return b.String()
}

// stateLine says the state and what it rests on.
func stateLine(rq *Request) string { return rq.State + " - " + rq.Why }
