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

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// shipCmd is the one way a change of the loop's goes in (loop.Ship). A loop run builds with prepare, check and
// propose, for its own case only; landing and rolling back are the loop's own steps, never a run's.
func shipCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"ship needs prepare, check, propose, status, land or rollback"}
	}
	run := os.Getenv("MOMENTUM_LOOP_RUN")
	switch args[0] {
	case "prepare", "check", "propose", "status":
	case "land", "rollback":
		if run != "" {
			return usageError{"a loop run builds and proposes; landing and rolling back are the loop's own steps"}
		}
	default:
		return usageError{fmt.Sprintf("unknown ship command %q", args[0])}
	}
	fs := flag.NewFlagSet("ship "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	caseID := fs.String("case", os.Getenv("MOMENTUM_LOOP_CASE"), "the case (in a loop run: its own, by default)")
	repo := fs.String("repo", "", "prepare: a checkout of the repository the change goes to")
	cfg := fs.String("config", "", "prepare: the configuration directory the change goes to")
	test := fs.String("test", "", "check: the test command, one program and its arguments, run without a shell")
	testFiles := fs.String("test-files", "", "check: comma-separated files of the change that hold the test (default: by their names)")
	title := fs.String("title", "", "propose: the change's title")
	bodyFile := fs.String("body-file", "", "propose: a file with the change's description")
	rollback := fs.String("rollback", "", "propose: how the change is undone")
	signal := fs.String("signal", "", `propose: the measurement's signal, {"source","kinds","tool","harness","match"}`)
	window := fs.String("window", "7d", "propose: the measurement's window (at least a day)")
	lesson := fs.String("lesson", "", "propose: the lesson the case teaches, one sentence")
	why := fs.String("why", "", "rollback: why")
	asJSON := fs.Bool("json", false, "status: the package as JSON")
	rest := args[1:]
	if (args[0] == "land" || args[0] == "rollback") && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		*caseID, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("ship %s takes no arguments, got %q", args[0], fs.Arg(0))}
	}
	if *caseID == "" {
		return usageError{"which case: --case ID"}
	}
	if run != "" && *caseID != os.Getenv("MOMENTUM_LOOP_CASE") {
		return usageError{fmt.Sprintf("this loop run works case %s, not %s", os.Getenv("MOMENTUM_LOOP_CASE"), *caseID)}
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	_, ship, err := setup(st, c.Loop)
	if err != nil {
		return err
	}
	var p loop.Package
	switch args[0] {
	case "status":
		pk, ok, err := ship.Load(*caseID)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(stdout, "%s: no package\n", *caseID)
			return nil
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(pk)
		}
		p = pk
	case "prepare":
		p, err = ship.Prepare(ctx, *caseID, *repo, *cfg)
	case "check":
		var files []string
		for _, f := range strings.Split(*testFiles, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
		p, err = ship.Check(ctx, *caseID, *test, files)
	case "propose":
		var pr loop.Proposal
		if pr.Window, err = parseSince(*window); err != nil {
			return usageError{err.Error()}
		}
		if *bodyFile != "" {
			b, err := os.ReadFile(*bodyFile)
			if err != nil {
				return usageError{err.Error()}
			}
			pr.Body = string(b)
		}
		if err := json.Unmarshal([]byte(*signal), &pr.Signal); err != nil {
			return usageError{"--signal is a JSON object: " + err.Error()}
		}
		pr.Title, pr.Rollback, pr.Lesson = *title, *rollback, *lesson
		p, err = ship.Propose(ctx, *caseID, pr)
	case "land":
		p, err = ship.Land(ctx, *caseID)
	case "rollback":
		if strings.TrimSpace(*why) == "" {
			return usageError{"ship rollback needs --why"}
		}
		p, err = ship.RollBack(ctx, *caseID, *why)
	}
	if errors.Is(err, loop.ErrRefused) || errors.Is(err, store.ErrBadCase) || errors.Is(err, store.ErrNoCase) {
		return usageError{err.Error()}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %s %s, %s\n", p.Case, p.Kind, p.State, firstText(p.PR, p.Work))
	if p.Check != nil {
		fmt.Fprintf(stdout, "  check: %s: exit %d without the change, %d with it\n", strings.Join(p.Check.Test, " "), p.Check.Before.Exit, p.Check.After.Exit)
	}
	for _, r := range p.Reviews {
		fmt.Fprintf(stdout, "  review %d: %d high, %d medium, %d low %s\n", r.Round, r.High, r.Medium, r.Low, r.Comment)
	}
	if !p.LandedAt.IsZero() {
		fmt.Fprintf(stdout, "  landed %s: %s\n", p.LandedAt.Local().Format(time.DateTime), strings.Join(p.Landed, " "))
	}
	return nil
}
