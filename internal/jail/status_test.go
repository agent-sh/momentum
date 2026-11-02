package jail

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusReportsOnlyTheSandboxedChild(t *testing.T) {
	for _, report := range []bool{false, true} {
		t.Run(fmt.Sprint(report), func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\n"
			if report {
				script += `test "$1" = --json-status-fd || exit 2
fd=$2
eval "printf '%s\n' '{\"child-pid\":7}' '{\"exit-code\":1}' >&$fd"
`
			}
			script += "echo 'synthetic mount setup failure' >&2\nexit 1\n"
			bwrap := filepath.Join(dir, "bwrap")
			if err := os.WriteFile(bwrap, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			var status *os.File
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd, err := (Spec{Bwrap: bwrap, Home: "/empty", Status: func(r *os.File) { status = r }}).Command(ctx, "true")
			if err != nil {
				t.Fatal(err)
			}
			defer status.Close()
			defer Close(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			Close(cmd) // the parent's writer must close or the reader would never reach EOF
			if err := cmd.Wait(); err == nil {
				t.Fatal("fake bubblewrap did not fail")
			}
			raw, err := io.ReadAll(status)
			if err != nil || strings.Contains(string(raw), `"child-pid"`) != report {
				t.Fatalf("status=%s err=%v", raw, err)
			}
		})
	}
}
