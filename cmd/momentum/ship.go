package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/config"
	"github.com/agent-sh/momentum/internal/loop"
	"github.com/agent-sh/momentum/internal/paths"
	"github.com/agent-sh/momentum/internal/store"
)

// shipCmd is the owner's interface to the host ship tool.
func shipCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"ship needs prepare, check, propose, status, land or rollback"}
	}
	switch args[0] {
	case "prepare", "check", "propose", "status":
	case "land", "rollback":

	default:
		return usageError{fmt.Sprintf("unknown ship command %q", args[0])}
	}
	fs := flag.NewFlagSet("ship "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	caseID := fs.String("case", "", "the case")
	repo := fs.String("repo", "", "prepare: a checkout of the repository the change goes to")
	test := fs.String("test", "", "check: the test command, one program and its arguments, run without a shell")
	testFiles := fs.String("test-files", "", "check: comma-separated files of the change that hold the test (default: by their names)")
	patchFile := fs.String("patch", "change.patch", "check, propose: cumulative patch against the package base")
	title := fs.String("title", "", "propose: the change's title")
	bodyFile := fs.String("body-file", "", "propose: a file with the change description")
	rollback := fs.String("rollback", "", "propose: how the change is undone")
	signal := fs.String("signal", "", `propose: the measurement's signal, {"source","kinds","tool","harness","match"}`)
	window := fs.String("window", "7d", "propose: the measurement's window (whole weeks: 7d, 14d and so on)")
	lesson := fs.String("lesson", "", "propose: the lesson the case teaches, one sentence")
	installed := fs.String("installed", "", "propose: the binary or installed configuration the machine runs; measured after the checkout first held the change and the file was rebuilt")
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
	c, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	ship := setupShip(st, c)
	abs, e := filepath.Abs(*patchFile)
	if e != nil {
		return e
	}
	patch := loop.Patch{Dir: filepath.Dir(abs), File: filepath.Base(abs)}
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
		p, err = ship.Prepare(ctx, *caseID, *repo)
	case "check":
		var files []string
		for _, f := range strings.Split(*testFiles, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
		p, err = ship.Check(ctx, *caseID, *test, files, patch)
	case "propose":
		var pr loop.Proposal
		if pr.Window, err = parseSince(*window); err != nil {
			return usageError{err.Error()}
		}
		if *bodyFile != "" {
			b, err := readBody(*bodyFile)
			if err != nil {
				return err
			}
			pr.Body = b
		}
		if err := json.Unmarshal([]byte(*signal), &pr.Signal); err != nil {
			return usageError{"--signal is a JSON object: " + err.Error()}
		}
		pr.Title, pr.Rollback, pr.Lesson, pr.Installed = *title, *rollback, *lesson, *installed
		p, err = ship.Propose(ctx, *caseID, pr, patch)
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
	if args[0] == "prepare" {
		fmt.Fprintf(stdout, "  base: %s; run clone: %s; ready for the next build run\n", p.BaseCommit, p.Work)
	}
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

// bodyMax is the longest description a proposal takes.
const bodyMax = 64 << 10

// readBody reads the owner's proposal description once.
func readBody(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", usageError{err.Error()}
	}
	defer f.Close()
	return readBodyFile(f)
}

func readBodyFile(f *os.File) (string, error) {
	b, err := loop.ReadFile(f, bodyMax)
	if err != nil {
		return "", usageError{err.Error()}
	}
	return string(b), nil
}
