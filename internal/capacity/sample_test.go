package capacity

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestABindMountsPathHasItsFilesystemsName(t *testing.T) {
	mi := filepath.Join(t.TempDir(), "mountinfo")
	write(t, mi, strings.Join([]string{
		"29 1 259:3 / / rw,relatime shared:1 - ext4 /dev/sda2 rw",
		"40 29 252:0 / /data rw,noatime shared:9 - ext4 /dev/mapper/data rw",
		"41 29 252:0 /cache /home/u/.cache rw,noatime shared:9 - ext4 /dev/mapper/data rw",
		`42 29 252:0 /ai\040ml /home/u/ai\040ml rw,noatime shared:9 - ext4 /dev/mapper/data rw`,
		"43 41 0:50 / /home/u/.cache/tmpfs rw - tmpfs tmpfs rw",
	}, "\n")+"\n")
	m := ReadMounts(mi)
	for in, want := range map[string]string{
		"/home/u/.cache/hf/hub/x":  "/data/cache/hf/hub/x", // through the bind mount
		"/home/u/.cache":           "/data/cache",
		"/data/cache/hf":           "/data/cache/hf",         // already its name
		"/home/u/ai ml/models":     "/data/ai ml/models",     // an escaped space
		"/home/u/projects/x":       "/home/u/projects/x",     // on the root filesystem
		"/home/u/.cache/tmpfs/a":   "/home/u/.cache/tmpfs/a", // another filesystem mounted on top
		"/home/u/.cache/../.cache": "/data/cache",
	} {
		if got := m.Canon(in); got != want {
			t.Errorf("Canon(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ReadMounts(filepath.Join(t.TempDir(), "none")).Canon("/a/b"); got != "/a/b" {
		t.Errorf("without a mount table a path is its own name, got %q", got)
	}
}

func TestTheSamplerSeesOpenMappedAndWorkingPathsAndRepoIDs(t *testing.T) {
	data := t.TempDir()
	proc := procRoot(t,
		fakeProc{pid: 10, ppid: 1, argv: []string{"/usr/bin/vllm", "serve", "--model=" + data + "/models/a", "Qwen/Qwen3-8B"},
			cwd: data + "/runs/r1", exe: "/usr/bin/vllm", fds: []string{data + "/models/b/w.bin", "socket:[1]", "/etc/hosts"},
			maps: []string{data + "/models/c/w.safetensors", data + "/models/c/w.safetensors", "/usr/lib/libc.so.6", data + "/models/d (deleted)"}},
		fakeProc{pid: 11, ppid: 10, argv: []string{"bash"}, cwd: "/home"},
	)
	s := TakeSample(proc, nil, func(p string) bool { return strings.HasPrefix(p, data+"/") })
	if s.Procs != 2 {
		t.Fatalf("read %d processes, want 2", s.Procs)
	}
	got := map[string]string{}
	for _, u := range s.Uses {
		got[u.Path] = u.How
	}
	want := map[string]string{
		data + "/models/a":               "arg",
		data + "/runs/r1":                "cwd",
		data + "/models/b/w.bin":         "fd",
		data + "/models/c/w.safetensors": "map",
		data + "/models/d":               "map", // a deleted file it still maps
	}
	for p, how := range want {
		if got[p] != how {
			t.Errorf("%s: seen as %q, want %q (all: %v)", p, got[p], how, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("kept %d paths, want only the %d under the roots: %v", len(got), len(want), got)
	}
	if u, ok := s.RepoIDs["Qwen/Qwen3-8B"]; !ok || u.PID != 10 {
		t.Errorf("the repository id on the command line: %v %v", u, ok)
	}
	if _, ok := s.InUse(data + "/models"); !ok {
		t.Error("a directory holding a used file is in use")
	}
	if _, ok := s.InUse(data + "/models/e"); ok {
		t.Error("a directory nothing uses is not in use")
	}
	if s.Table[11].PPID != 10 || !s.Children(10) || s.Children(11) {
		t.Errorf("process table: %+v", s.Table)
	}
}

func TestWordsFindPathsInUnitsAndCommandLines(t *testing.T) {
	home := homeDir()
	got := words(`ExecStart=-%h/bin/serve --model "/data/m/x y" -c 512` + "\nEnvironmentFile=-/etc/a.env\nPATH=/usr/bin:~/bin")
	for _, w := range []string{home + "/bin/serve", "/data/m/x", "/etc/a.env", "/usr/bin", home + "/bin"} {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("%q not among %q", w, got)
		}
	}
	for w, want := range map[string]bool{"Qwen/Qwen3-8B": true, "a/b/c": false, "/abs/x": false, "x--y/z": false, "-x/y": false, "org/m.gguf": true} {
		if isRepoID(w) != want {
			t.Errorf("isRepoID(%q) = %v", w, !want)
		}
	}
}
