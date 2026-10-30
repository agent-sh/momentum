package supervise

import (
	"strings"
	"testing"
	"time"
)

func TestErrorClassSaysWhetherToTryAgain(t *testing.T) {
	cases := map[string]string{
		"API Error: 503 Bedrock is unable to process your request. This is a server-side issue, usually temporary.": Transient,
		"API Error: Can't reach the API server — check your internet or DNS (EAI_AGAIN)":                            Transient,
		"API Error: Server error mid-response. The response above may be incomplete.":                               Transient,
		"API Error: 500 The system encountered an unexpected error during processing.":                              Transient,
		"API Error: Connection refused — a firewall or proxy may be blocking it":                                    Transient,
		"API Error: 529 overloaded":                                        Transient,
		"Login expired · Please run /login":                                Login,
		"Not logged in · Please run /login":                                Login,
		"You've hit your weekly limit · resets Mar 7, 9am (Europe/Lisbon)": Limit,
		"5-hour limit reached ∙ resets 3pm":                                Limit,
		"rate_limit: No response requested.":                               Limit,
		"Your organization has disabled Claude subscription access for Claude Code · Use an Anthropic API key": Org,
		"API Error: The model's safeguards flagged this message (https://example.com/usage-policy)":            "",
		"Prompt is too long": "",
		"Prompt is too long · automatic compaction failed: API Error: 503 Bedrock is unable to process": "",
		"":                   "",
		"some other failure": "",
	}
	for text, want := range cases {
		if got := ErrorClass(text); got != want {
			t.Errorf("ErrorClass(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestResetAtReadsTheTimeALimitStates(t *testing.T) {
	lis, err1 := time.LoadLocation("Europe/Lisbon")
	ny, err2 := time.LoadLocation("America/New_York")
	if err1 != nil || err2 != nil {
		t.Skip("no zone data")
	}
	at := time.Date(2026, 3, 4, 9, 28, 0, 0, lis)
	cases := []struct {
		text string
		at   time.Time
		want time.Time
		ok   bool
	}{
		{"You've hit your weekly limit · resets Mar 7, 9am (Europe/Lisbon)", at, time.Date(2026, 3, 7, 9, 0, 0, 0, lis), true},
		{"limit reached · resets Mar 7, 7:30pm (Europe/Lisbon)", at, time.Date(2026, 3, 7, 19, 30, 0, 0, lis), true},
		{"5-hour limit reached ∙ resets 3pm (Europe/Lisbon)", at, time.Date(2026, 3, 4, 15, 0, 0, 0, lis), true},
		{"limit reached ∙ resets 8am (Europe/Lisbon)", at, time.Date(2026, 3, 5, 8, 0, 0, 0, lis), true}, // the next 8am
		{"limit · resets at 15:30 (Europe/Lisbon)", at, time.Date(2026, 3, 4, 15, 30, 0, 0, lis), true},
		{"5-hour limit reached ∙ resets 3pm America/New_York", at, time.Date(2026, 3, 4, 15, 0, 0, 0, ny), true},
		{"limit reached ∙ resets at 23:00 UTC", at, time.Date(2026, 3, 4, 23, 0, 0, 0, time.UTC), true},
		{"limit reached · resets Jan 2, 9am (Europe/Lisbon)", time.Date(2026, 12, 30, 8, 0, 0, 0, lis), time.Date(2027, 1, 2, 9, 0, 0, 0, lis), true},
		{"Claude AI usage limit reached|1772618400", time.Unix(1772618400-3600, 0), time.Unix(1772618400, 0), true},
		{"Claude AI usage limit reached|1772618400", time.Unix(1772618400+3600, 0), time.Time{}, false}, // before the error
		// a date before the error is a misreading, and a zone it does not know gives no time
		{"limit reached · resets Mar 4, 8am (Europe/Lisbon)", at, time.Time{}, false},
		{"limit reached · resets Feb 20, 8am (Europe/Lisbon)", at, time.Time{}, false},
		{"limit reached ∙ resets 3pm PST", at, time.Time{}, false},
		{"limit reached ∙ resets 3pm (Mars/Olympus)", at, time.Time{}, false},
		{"You've hit your weekly limit", at, time.Time{}, false},
		{"resets in a while", at, time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := ResetAt(c.text, c.at)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("ResetAt(%q) = %v %v, want %v %v", c.text, got, ok, c.want, c.ok)
		}
	}
}

func TestPromiseFindsWorkLeftForLater(t *testing.T) {
	cases := []struct {
		words string
		quote string // "" for none
	}{
		{"The import is at row 9000. I'll report when the three tables are loaded, in about an hour.", "I'll report when the three tables are loaded, in about an hour."},
		{"Saved the draft. The upload is still going, and I'll be notified when it finishes.", "The upload is still going, and I'll be notified when it finishes."},
		{"Drafting the summary now. The outline comes first.", "Drafting the summary now."},
		{"The ticket also asks for a smoke test. Running it.", "Running it."},
		{"The lint step failed. I’m checking that failure before tagging.", "I’m checking that failure before tagging."},
		{"Tagged the release. Next check at 14:10.", "Next check at 14:10."},
		{"Only the parser half is done. Next step: wire the CLI flags.", "Next step: wire the CLI flags."},
		{"Waiting for the index build to finish.", "Waiting for the index build to finish."},
		// done, or asking the owner
		{"All four checks pass and the branch is pushed.", ""},
		{"Done. Nothing else is left.", ""},
		{"The migration plan is written. Should I apply it?", ""},
		{"If the export looks wrong, tell me which rows and I'll fix it.", ""},
		{"Both items are new scope; say go and I'll start them.", ""},
		{"Once you confirm, I'll book the staging slot.", ""},
		{"I'll pick the refactor back up when you're done with the review.", ""},
		{"Does the new chart render on your screen now?", ""},
	}
	for _, c := range cases {
		if got := Promise(Words(c.words)); got != c.quote {
			t.Errorf("Promise(%q) = %q, want %q", c.words, got, c.quote)
		}
	}
}

func TestQuestionIsWhatTheTurnAsksTheOwner(t *testing.T) {
	q := "Should I drop those two rows from the import, or keep them and fill the owner in from the old table?"
	long := strings.Repeat("The table holds the rows we loaded today, and every column matches the schema. ", 4) +
		"Two of the rows have no owner, which the old importer allowed. " + q
	if got := Question(long); got != q {
		t.Fatalf("question %q", got)
	}
	if got := Asked(Words(long)); got == "" || len(got) >= len(q) {
		t.Fatalf("the words keep only the end of it: %q", got)
	}
	if got := Question("Merged and tagged. Nothing else is left."); got != "" {
		t.Fatalf("no question: %q", got)
	}
}

func TestWordsKeepWhatTheStoreKeeps(t *testing.T) {
	long := strings.Repeat("start ", 40) + strings.Repeat("middle ", 60) + "and the end, I'll report back."
	w := Words(long)
	if !strings.HasPrefix(w, "start start") || !strings.HasSuffix(w, "I'll report back.") || !strings.Contains(w, "… ") ||
		len(w) > headChars+tailChars+8 {
		t.Fatalf("words %q", w)
	}
	if got := Words("short  text\nhere"); got != "short text here" {
		t.Fatalf("short %q", got)
	}
	// A Codex memory citation after the agent's words is not its words.
	if got := Promise(Words("I'm checking the config loader before changing its defaults. <oai-mem-citation><citation_entries>a guidance] </citation_entries><rollout_ids> </rollout_ids></oai-mem-citation>")); got != "I'm checking the config loader before changing its defaults." {
		t.Fatalf("with a citation: %q", got)
	}
}

func TestHermesNotices(t *testing.T) {
	cut := "The model's reply was cut off before it finished (it hit its output length limit), so Hermes didn't run the incomplete action. Nothing was changed. Send `continue`, ask for the work in smaller steps, or raise max_tokens for this model."
	if !Cutoff(Words(cut)) || Cutoff("Sent the release notes.") {
		t.Fatal("cutoff")
	}
	short := "Your request was not processed. Send it again if you still want me to carry it out."
	if !CutShort(Words(short)) || !CutShort("This turn did not complete. Some actions may already have run; verify their effects before resending.") || CutShort(cut) {
		t.Fatal("cut short")
	}
}
