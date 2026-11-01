package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/plain"
	"github.com/agent-sh/momentum/internal/store"
)

// caseCmd keeps the cases: one answer in progress each, to one of the standing questions. A detector or an agent
// opens one by its signature; the investigation records its mechanism, alternatives, next test and benefit; the
// ship and measure tools move it through deployed and measuring; it closes with a reason.
func caseCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"case needs list, show, open, set, evidence, signature, close or reopen"}
	}
	switch args[0] {
	case "list":
		return caseList(ctx, args[1:], stdout, stderr)
	case "show":
		return caseShow(ctx, args[1:], stdout, stderr)
	case "open":
		return caseOpen(ctx, args[1:], stdout, stderr)
	case "set":
		return caseSet(ctx, args[1:], stdout, stderr)
	case "evidence":
		return caseEvidence(ctx, args[1:], stdout, stderr)
	case "signature":
		return caseSignature(ctx, args[1:], stdout, stderr)
	case "close":
		return caseClose(ctx, args[1:], stdout, stderr)
	case "reopen":
		return caseReopen(ctx, args[1:], stdout, stderr)
	}
	return usageError{fmt.Sprintf("unknown case command %q", args[0])}
}

// caseActor is who a change is logged as: $MOMENTUM_ACTOR (the loop sets its run), else the command line.
func caseActor() string {
	if a := plain.Line(os.Getenv("MOMENTUM_ACTOR")); a != "" {
		return a
	}
	return "cli"
}

// caseErr turns the store's refusals into usage errors, which exit 2 with the reason.
func caseErr(err error) error {
	if errors.Is(err, store.ErrBadCase) || errors.Is(err, store.ErrNoCase) {
		return usageError{err.Error()}
	}
	return err
}

// idThenFlags splits a command's leading case id from its flags: the flag package stops at the first argument that
// is not a flag.
func idThenFlags(name string, args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, usageError{name + " takes a case's id first"}
	}
	return args[0], args[1:], nil
}

// refs collects --evidence REF, once per flag.
type refs []string

func (r *refs) String() string { return strings.Join(*r, ",") }
func (r *refs) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// listedCase is a case as list and show print it with --json.
type listedCase struct {
	ID             string   `json:"id"`
	Family         string   `json:"family"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	Reason         string   `json:"reason,omitempty"`
	Into           string   `json:"into,omitempty"`
	Mechanism      string   `json:"mechanism,omitempty"`
	Alternatives   []string `json:"alternatives"`
	NextTest       string   `json:"next_test,omitempty"`
	Benefit        string   `json:"benefit,omitempty"`
	CostUSD        float64  `json:"cost_usd"`
	CostTokens     int64    `json:"cost_tokens"`
	CostSeconds    float64  `json:"cost_seconds"`
	Runs           int      `json:"runs"`
	Evidence       int      `json:"evidence"`
	ClosedEvidence int      `json:"closed_evidence,omitempty"`
	Opened         string   `json:"opened"`
	Updated        string   `json:"updated"`
	Seen           string   `json:"seen"`
	Closed         string   `json:"closed,omitempty"`
	Deployed       string   `json:"deployed,omitempty"`
	Package        string   `json:"package,omitempty"`
	Measurement    *struct {
		WindowHours float64 `json:"window_hours"`
		Before      int     `json:"before"`
		After       int     `json:"after"`
		Effect      string  `json:"effect"`
	} `json:"measurement,omitempty"`
	Signatures []string `json:"signatures"`
}

func caseJSON(c store.Case) listedCase {
	l := listedCase{ID: c.ID, Family: c.Family, Title: c.Title, Status: string(c.Status), Reason: string(c.Reason), Into: c.Into,
		Mechanism: c.Mechanism, Alternatives: c.Alternatives, NextTest: c.NextTest, Benefit: c.Benefit, CostUSD: c.Cost.USD,
		CostTokens: c.Cost.Tokens, CostSeconds: c.Cost.Duration.Seconds(), Runs: c.Cost.Runs, Evidence: c.Evidence,
		ClosedEvidence: c.ClosedEvidence, Opened: rfc3339(c.Opened), Updated: rfc3339(c.Updated), Seen: rfc3339(c.Seen),
		Package: c.Package, Signatures: c.Signatures}
	if l.Alternatives == nil {
		l.Alternatives = []string{}
	}
	if l.Signatures == nil {
		l.Signatures = []string{}
	}
	if !c.Closed.IsZero() {
		l.Closed = rfc3339(c.Closed)
	}
	if !c.Deployed.IsZero() {
		l.Deployed = rfc3339(c.Deployed)
	}
	if m := c.Measurement; m.Window > 0 || m.Effect != "" {
		l.Measurement = &struct {
			WindowHours float64 `json:"window_hours"`
			Before      int     `json:"before"`
			After       int     `json:"after"`
			Effect      string  `json:"effect"`
		}{m.Window.Hours(), m.Before, m.After, m.Effect}
	}
	return l
}

// statusLine is a case's status for a terminal: closed ones with their reason.
func statusLine(c store.Case) string {
	switch {
	case c.Status != store.CaseClosed:
		return string(c.Status)
	case c.Reason == store.Duplicate:
		return "closed: duplicate of " + c.Into
	}
	return "closed: " + string(c.Reason)
}

func caseList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("case list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	family := fs.String("family", "", "one family: "+strings.Join(store.Families, ", "))
	status := fs.String("status", "", "comma-separated statuses (default: every one but closed)")
	all := fs.Bool("all", false, "closed cases too")
	asJSON := fs.Bool("json", false, "one JSON object per case")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("case list takes no arguments, got %q", fs.Arg(0))}
	}
	f := store.CaseFilter{Family: *family, All: *all}
	if *status != "" {
		for _, s := range strings.Split(*status, ",") {
			f.Statuses = append(f.Statuses, store.CaseStatus(strings.TrimSpace(s)))
		}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	cs, err := st.Cases(ctx, f)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	for _, c := range cs {
		if *asJSON {
			if err := enc.Encode(caseJSON(c)); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(stdout, "%-9s %-16s %-14s %4d ev  $%6.2f  %s  %s\n", c.ID, c.Family, statusLine(c), c.Evidence, c.Cost.USD,
			c.Updated.Local().Format("01-02 15:04"), c.Title)
	}
	return nil
}

// shownCase is a case as show --json prints it: the case, its newest evidence and its log.
type shownCase struct {
	listedCase
	Recent []shownEvidence `json:"recent_evidence"`
	Log    []shownEntry    `json:"log"`
}

type shownEvidence struct {
	Ref     string `json:"ref"`
	At      string `json:"at"`
	Kind    string `json:"kind,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type shownEntry struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

func caseShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	id, rest, err := idThenFlags("case show", args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("case show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("evidence", 20, "how much of its newest evidence to print (0: all)")
	asJSON := fs.Bool("json", false, "one JSON object")
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := st.CaseByID(ctx, id)
	if err != nil {
		return caseErr(err)
	}
	evs, err := st.CaseEvidence(ctx, c.ID, *n)
	if err != nil {
		return err
	}
	log, err := st.CaseLog(ctx, c.ID)
	if err != nil {
		return err
	}
	if *asJSON {
		out := shownCase{listedCase: caseJSON(c), Recent: []shownEvidence{}, Log: []shownEntry{}}
		for _, e := range evs {
			out.Recent = append(out.Recent, shownEvidence{Ref: e.Ref, At: rfc3339(e.At), Kind: e.Kind, Summary: e.Summary})
		}
		for _, e := range log {
			out.Log = append(out.Log, shownEntry{At: rfc3339(e.At), Actor: e.Actor, Action: e.Action, Detail: e.Detail})
		}
		return json.NewEncoder(stdout).Encode(out)
	}
	fmt.Fprintf(stdout, "%s  %s  [%s, %s]\n", c.ID, c.Title, c.Family, statusLine(c))
	field := func(name, v string) {
		if v != "" {
			fmt.Fprintf(stdout, "  %-13s %s\n", name+":", v)
		}
	}
	field("mechanism", c.Mechanism)
	for i, a := range c.Alternatives {
		field(fmt.Sprintf("alternative %d", i+1), a)
	}
	field("next test", c.NextTest)
	field("benefit", c.Benefit)
	field("cost", fmt.Sprintf("$%.2f, %d tokens, %s over %d runs", c.Cost.USD, c.Cost.Tokens, c.Cost.Duration.Round(time.Second), c.Cost.Runs))
	field("signatures", strings.Join(c.Signatures, " | "))
	if !c.Deployed.IsZero() {
		field("deployed", c.Deployed.Local().Format("2006-01-02 15:04")+" "+c.Package)
	}
	if m := c.Measurement; m.Window > 0 || m.Effect != "" {
		field("measured", fmt.Sprintf("%d before, %d after over %s each: %s", m.Before, m.After, m.Window, m.Effect))
	}
	fmt.Fprintf(stdout, "  evidence: %d, newest first\n", c.Evidence)
	for _, e := range evs {
		fmt.Fprintf(stdout, "    %s  %-14s %s  %s\n", e.At.Local().Format("01-02 15:04"), e.Kind, e.Ref, plain.Clip(e.Summary, 120))
	}
	fmt.Fprintln(stdout, "  log:")
	for _, e := range log {
		fmt.Fprintf(stdout, "    %s  %-10s %-12s %s\n", e.At.Local().Format("01-02 15:04"), e.Actor, e.Action, e.Detail)
	}
	return nil
}

// printCase prints what a change left: the case's id, status and title, and what happened.
func printCase(stdout io.Writer, c store.Case, what string) {
	fmt.Fprintf(stdout, "%s: %s [%s] %s\n", c.ID, what, statusLine(c), c.Title)
}

func caseOpen(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("case open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	family := fs.String("family", "", "its family: "+strings.Join(store.Families, ", "))
	sig := fs.String("signature", "", "what it is known by; a case that holds it already takes the evidence instead")
	title := fs.String("title", "", fmt.Sprintf("one plain line, at most %d characters", store.TitleMax))
	var evs refs
	fs.Var(&evs, "evidence", "a reference (ev:EVENT, note:ID, git:OWNER/REPO@SHA, file:PATH, an address); repeat for more")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("case open takes no arguments, got %q", fs.Arg(0))}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	f := store.Finding{Family: *family, Signature: *sig, Title: *title}
	for _, r := range evs {
		f.Evidence = append(f.Evidence, store.Evidence{Ref: r})
	}
	c, obs, err := st.ObserveCase(ctx, f, caseActor())
	if err != nil {
		return caseErr(err)
	}
	printCase(stdout, c, string(obs))
	return nil
}

func caseSet(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	id, rest, err := idThenFlags("case set", args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("case set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	status := fs.String("status", "", "open, investigating or testing (testing needs a mechanism and a next test)")
	var alts refs
	fs.Var(&alts, "alternative", "another mechanism that could explain the evidence; repeat for more (replaces the list)")
	texts := map[string]*string{}
	for name, usage := range map[string]string{"title": "its title", "mechanism": "the suspected mechanism",
		"next-test": "the next test that tells the mechanism from the alternatives", "benefit": "the expected benefit"} {
		texts[name] = fs.String(name, "", usage)
	}
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("case set takes one case's id, got %q too", fs.Arg(0))}
	}
	u := store.CaseUpdate{Status: store.CaseStatus(*status)}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	pick := func(name string) *string {
		if set[name] {
			return texts[name]
		}
		return nil
	}
	u.Title, u.Mechanism, u.NextTest, u.Benefit = pick("title"), pick("mechanism"), pick("next-test"), pick("benefit")
	if set["alternative"] {
		u.Alternatives = alts
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := st.UpdateCase(ctx, id, u, caseActor())
	if err != nil {
		return caseErr(err)
	}
	printCase(stdout, c, "set")
	return nil
}

func caseEvidence(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	id, rest, err := idThenFlags("case evidence", args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("case evidence", flag.ContinueOnError)
	fs.SetOutput(stderr)
	at := fs.String("at", "", "when it happened, for a reference that is not an event (default: now)")
	kind := fs.String("kind", "", "what it is, for a reference that is not an event")
	summary := fs.String("summary", "", "a short line about it, for a reference that is not an event")
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() == 0 {
		return usageError{"case evidence takes a case's id, its flags, then one or more references"}
	}
	var when time.Time
	if *at != "" {
		if when, err = parseWhen(*at); err != nil {
			return usageError{err.Error()}
		}
	}
	var evs []store.Evidence
	for _, r := range fs.Args() {
		evs = append(evs, store.Evidence{Ref: r, At: when, Kind: *kind, Summary: *summary})
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	c, obs, err := st.AddCaseEvidence(ctx, id, evs, caseActor())
	if err != nil {
		return caseErr(err)
	}
	printCase(stdout, c, string(obs))
	return nil
}

func caseSignature(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return usageError{"case signature takes a case's id and a signature (the root cause's: root:KEY)"}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	c, merged, err := st.AddCaseSignature(ctx, args[0], args[1], caseActor())
	if err != nil {
		return caseErr(err)
	}
	what := "signature added"
	if merged {
		what = fmt.Sprintf("%s went into this case, which holds the signature: work on here", args[0])
	}
	printCase(stdout, c, what)
	return nil
}

func caseClose(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	id, rest, err := idThenFlags("case close", args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("case close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reason := fs.String("reason", "", "not-worth-it, insufficient-evidence, already-solved or duplicate (improved is the measure tool's)")
	into := fs.String("into", "", "for a duplicate: the case with the same root cause")
	why := fs.String("why", "", "why, in a short line")
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("case close takes one case's id, got %q too", fs.Arg(0))}
	}
	if strings.TrimSpace(*why) == "" {
		return usageError{"case close needs --why"}
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := st.CloseCase(ctx, id, store.CloseReason(*reason), *into, *why, caseActor())
	if err != nil {
		return caseErr(err)
	}
	what := "closed"
	if c.ID != id {
		what = id + " went into this case"
	}
	printCase(stdout, c, what)
	return nil
}

func caseReopen(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	id, rest, err := idThenFlags("case reopen", args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("case reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var evs refs
	fs.Var(&evs, "evidence", "a reference the case does not hold; repeat for more")
	why := fs.String("why", "", "how the evidence changed, in a short line")
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("case reopen takes one case's id, got %q too", fs.Arg(0))}
	}
	if len(evs) == 0 || strings.TrimSpace(*why) == "" {
		return usageError{"case reopen needs --evidence and --why: a closed case comes back only on evidence it does not hold"}
	}
	var es []store.Evidence
	for _, r := range evs {
		es = append(es, store.Evidence{Ref: r})
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := st.ReopenCase(ctx, id, es, *why, caseActor())
	if err != nil {
		return caseErr(err)
	}
	printCase(stdout, c, "reopened")
	return nil
}
