package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceGitRefusesRunPathsBeforeInvokingGit(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	runDir := filepath.Join(loopDir(), "runs", "fixture")
	work := filepath.Join(loopDir(), "work", "fixture")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	// A synthetic Git program is the canary: even a configuration query is a failure here.
	marker := filepath.Join(dir, "marker")
	program := filepath.Join(dir, "git")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("MOMENTUM_LOOP_RUN", "fixture")
	t.Setenv("MOMENTUM_LOOP_DIR", runDir)
	// Even a broad read root does not admit the loop's writable directories.
	t.Setenv("MOMENTUM_LOOP_READ", filepath.Dir(loopDir()))
	var out, diag bytes.Buffer
	for _, request := range []string{work, runDir, filepath.Join(work, "nested")} {
		err := evidenceGit(context.Background(), []string{request, "log"}, &out, &diag)
		if _, markerErr := os.Stat(marker); markerErr == nil {
			t.Fatal("host invoked Git before rejecting the run path")
		}
		if err == nil || !strings.Contains(err.Error(), "never reads") {
			t.Fatalf("run path reached evidence Git: %v", err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("host invoked Git before rejecting the run path")
	}
}

func TestProposalBodyResolvesUnderTheRunRootFromAHostDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "body.md"), []byte("marker-7f3a"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOMENTUM_LOOP_DIR", dir)
	b, err := readBody("body.md", "fixture")
	if err != nil || b != "marker-7f3a" {
		t.Fatalf("relative body did not use its confined root: %q %v", b, err)
	}
}

func TestEvidenceRecordsNeverReadWritableClonesCachesOrExports(t *testing.T) {
	isolatedHome(t)
	for _, name := range []string{"work", "cache", "exports"} {
		file := filepath.Join(loopDir(), name, "fixture", "record")
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("marker-7f3a"), 0600); err != nil {
			t.Fatal(err)
		}
		if f, err := openRecord(file); err == nil {
			f.Close()
			t.Fatal("evidence read a writable clone, cache or export")
		}
	}
}

func TestRecordsOfAnEarlierRunRemainRootConfined(t *testing.T) {
	isolatedHome(t)
	dir := filepath.Join(loopDir(), "runs", "earlier")
	outside := t.TempDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "record"), []byte("marker-7f3a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "parent")); err != nil {
		t.Fatal(err)
	}
	if f, err := openRecord(filepath.Join(dir, "parent", "record")); err == nil {
		f.Close()
		t.Fatal("an earlier run's intermediate link escaped its root")
	}
}
