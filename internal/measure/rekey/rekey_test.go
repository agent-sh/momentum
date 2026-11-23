package rekey

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

// A replay's output reads back as it was written, and a diff of two builds' keys says which events each re-keys,
// drops and adds, and which drops are not the case's own.
func TestTheKeysOfTwoBuildsCompare(t *testing.T) {
	var b bytes.Buffer
	since := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	if err := Write(&b, Header{Since: since, Until: since.Add(7 * 24 * time.Hour)}, []Line{{"e1", "a"}, {"e2", "a"}, {"e3", "b"}, {"e1", "z"}}); err != nil {
		t.Fatal(err)
	}
	h, before, err := Read(b.Bytes())
	if err != nil || !h.Since.Equal(since) || len(before) != 3 || before["e1"] != "a" {
		t.Fatalf("read back: %+v %v %v", h, before, err)
	}
	after := map[string]string{"e1": "a2", "e2": "a", "e4": "c"}
	d := Compare(before, after)
	if d.Before != 3 || d.After != 3 || !slices.Equal(d.Rekeyed, []Move{{"a", "a2", 1}}) || !slices.Equal(d.Dropped, []Count{{"b", 1}}) || !slices.Equal(d.Added, []Count{{"c", 1}}) {
		t.Fatalf("diff: %+v", d)
	}
	if f := d.Foreign([]string{"a"}); !slices.Equal(f, []Count{{"b", 1}}) {
		t.Fatalf("foreign drops: %v", f)
	}
	if f := d.Foreign([]string{"a", "b"}); len(f) != 0 {
		t.Fatalf("the case's own drops are not foreign: %v", f)
	}
	if r, dr := d.Touches([]string{"a"}); r != 1 || dr != 0 {
		t.Fatalf("touches a: %d %d", r, dr)
	}
	for name, out := range map[string]string{"empty": "", "no header": `{"id":"e1","sig":"a"}` + "\n", "another format": `{"replay":2}` + "\n",
		"a broken line": `{"replay":1}` + "\n{\n", "an empty id": `{"replay":1}` + "\n" + `{"id":"","sig":"a"}` + "\n"} {
		if _, _, err := Read([]byte(out)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if _, _, err := Read([]byte(strings.Repeat("x", MaxOutput+1))); err == nil {
		t.Error("an output over the bound was read")
	}
}
