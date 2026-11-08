package main

import (
	"os"
	"path/filepath"

	"testing"
)

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
		if f, _, err := openRecord(file); err == nil {
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
	if f, _, err := openRecord(filepath.Join(dir, "parent", "record")); err == nil {
		f.Close()
		t.Fatal("an earlier run's intermediate link escaped its root")
	}
}
