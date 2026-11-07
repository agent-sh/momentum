package supervise

import "testing"

// A sessions run keys a session by its harness's key; the store files its events under the id its transcripts
// carry. Each harness's key reads as that id.
func TestEachHarnesssKeyIsTheStoresSession(t *testing.T) {
	cases := []struct{ tool, key, transcript, want string }{
		{"claude", "6f1e2d3c-aaaa-4bbb-8ccc-0d1e2f3a4b5c", "", "6f1e2d3c-aaaa-4bbb-8ccc-0d1e2f3a4b5c"},
		{"claude", "job-7f3e", "/work/claude/projects/-work/6f1e2d3c-aaaa-4bbb-8ccc-0d1e2f3a4b5c.jsonl", "6f1e2d3c-aaaa-4bbb-8ccc-0d1e2f3a4b5c"},
		{"codex", "019a0000-0000-7000-8000-000000000001", "", "019a0000-0000-7000-8000-000000000001"},
		{"hermes", "hermes:20261107_100000_abc123", "", "20261107_100000_abc123"},
		{"opencode", "opencode:ses_0001", "", "ses_0001"},
		{"pi", "pi:2026-11-07T10-00-00-000Z_0b9c7d2e-1111-4222-8333-944455556666", "", "0b9c7d2e-1111-4222-8333-944455556666"},
		{"pi", "pi:2026-11-07T10-00-00-000Z_0b9c7d2e-1111-4222-8333-944455556666",
			"/work/pi/sessions/--work--/2026-11-07T10-00-00-000Z_0b9c7d2e-1111-4222-8333-944455556666.jsonl", "0b9c7d2e-1111-4222-8333-944455556666"},
	}
	for _, c := range cases {
		if got := StoreSessionOf(c.tool, c.key, c.transcript); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.tool, c.key, got, c.want)
		}
	}
}
