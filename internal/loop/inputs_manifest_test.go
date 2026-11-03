package loop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInputsManifestJSONAndSummary(t *testing.T) {
	end := time.Date(2025, 4, 10, 12, 0, 0, 0, time.UTC)
	m := InputsManifest{Store: StoreManifest{Since: end.Add(-StoreWindow), Until: end, Events: 41203, Notes: 3, Canary: true},
		Records: RecordsManifest{Written: 12}, Configuration: ConfigManifest{Files: make([]ConfigFile, 87), Bytes: 1468006},
		Repositories: []RepoInput{{Name: "one", Commit: "abc", Files: 2, Bytes: 1 << 20}, {Name: "two", Error: "refused"}},
		Tacit:        RepoInput{Commit: "abc123456789", Files: 9}}
	want := "store copy 30 days, 41,203 events; 12 records; configuration 87 files, 1.4 MiB; 2 repositories (one 2 files, 1.0 MiB at abc, log 0 commits; two absent: refused); tacit 9 files, 0.0 MiB at abc1234, log 0 commits; no diffs"
	if got := m.Summary(); got != want || strings.ContainsAny(got, "\n\r") {
		t.Fatalf("summary %q", got)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(b, &fields); err != nil || len(fields) != 7 {
		t.Fatal("manifest is not one JSON object with seven inputs")
	}
	for _, key := range []string{"store", "records", "configuration", "repositories", "tacit", "diffs", "programs"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("manifest omitted %s", key)
		}
	}
	var roundtrip InputsManifest
	if err = json.Unmarshal(b, &roundtrip); err != nil || roundtrip.Summary() != want {
		t.Fatalf("manifest roundtrip: %v", err)
	}
}
