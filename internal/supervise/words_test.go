package supervise

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/momentum/internal/source"
)

func TestErrorClassSaysWhetherToTryAgain(t *testing.T) {
	cases := map[string]string{
		"API Error: 503 Bedrock is unable to process your request. This is a server-side issue, usually temporary.": Transient,
		"API Error: Can't reach the API server — check your internet or DNS (EAI_AGAIN)":                            Transient,
		"API Error: Server error mid-response. The response above may be incomplete.":                               Transient,
		"API Error: 500 The system encountered an unexpected error during processing.":                              Transient,
		"API Error: Connection refused — a firewall or proxy may be blocking it":                                    Transient,
		"API Error: 529 overloaded":                                           Transient,
		"Login expired · Please run /login":                                   Login,
		"Please run /login · API Error: 403 Attention Required! | Cloudflare": Transient, // the edge's page, not the login
		// The harness's own words for a request the edge blocked say the network must change: not tried again blindly.
		"API Error: Request blocked before it reached Anthropic's API (HTTP 403) · the network this request came from isn't allowed · signing in again won't change this · check your VPN or network and retry": "",
		"Please run /login · API Error: 403 Just a moment...":                                                  Login, // another page of the edge: not known to pass by itself
		"Not logged in · Please run /login":                                                                    Login,
		"You've hit your weekly limit · resets Mar 7, 9am (Europe/Lisbon)":                                     Limit,
		"5-hour limit reached ∙ resets 3pm":                                                                    Limit,
		"rate_limit: No response requested.":                                                                   Limit,
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
		// a mark inside a token ends no sentence: the offer stays whole and is none
		{"If you approve plan v2.1 in docs/plan.md, I'll build the page.", ""},
		{"Say the word and after the edit to config.yaml I'll run the import.", ""},
		{"If you prefer the short one (e.g. two columns), I'll build the page.", ""},
		// and a promise with one is quoted whole
		{"The import runs. I'll check config.yaml again when it ends.", "I'll check config.yaml again when it ends."},
		{"The import runs. I'll check both files (e.g. config.yaml) when it ends.", "I'll check both files (e.g. config.yaml) when it ends."},
		// a sentence that opens with a small letter joins the one before for an ask, and is its own for a promise:
		// the promise is looked for in the last two, no further back
		{"I'll rerun the suite after the rebase. The rebase went through. go test passes now.", ""},
		{"I'll check foo.go now. foo.go is fine. bar.go has the fix.", ""},
		{"The run is green. checking the logs next.", "checking the logs next."},
		{"All done. checking the logs next.", "checking the logs next."},
		{"The report is long. I'll check the two files (e.g. config.yaml) when it ends.", "I'll check the two files (e.g. config.yaml) when it ends."},
	}
	for _, c := range cases {
		if got := Promise(Words(c.words)); got != c.quote {
			t.Errorf("Promise(%q) = %q, want %q", c.words, got, c.quote)
		}
	}
	// The store kept a long text by its first 160 and last 80 bytes, and still keeps so an end whose last sentence
	// does not fit (byEnds). A sentence whose start was cut may hang on what is gone: an offer that waits for the
	// owner reads as a promise once "If you approve it," or "Say the word" is cut. It is none.
	long := strings.Repeat("The report lists every table and its row count. ", 6)
	for _, c := range []struct{ end, tail, quote string }{
		{"Say the word on either and I'll make the change to the staging rule and to the ads.", "the word on either and I'll make", ""},
		{"If you approve it, I'll build the checklist page and both of the ads in two languages.", "approve it, I'll build", ""},
		// the rest of a cut sentence is cut with it, whatever marks it holds
		{"If you approve the plan (v2.1), I'll build the checklist page and both ads in two languages.", "ve the plan (v2.1), I'll build", ""},
		{"If you want it, I can also change internal/report/table.go and then I'll run the import again.", ", I can also change internal/report/table.go and then I'll run", ""},
		// a capital letter is no start of a sentence: the cut fell on "I'll" here
		{"If you approve it, I'll build the checklist page and both of the ads in two languages, as planned.", "I'll build the checklist", ""},
		// a sentence that fills the kept end to the byte reads the same, and is lost
		{"I'll report the three row counts as soon as the import of the last table is run.", "I'll report the three", ""},
		// a kept end that opens with the mark that closed the sentence before: that mark is its first sentence
		{"I'll report the three row counts as soon as the import of the last table ends.", ". I'll report the three", "I'll report the three row counts as soon as the import of the last table ends."},
		// a whole sentence after the cut one is read as before, the last and the one before it
		{"The long export step that started this morning has now finished without any error at all. I'll check back at 14:10.", "his morning has now", "I'll check back at 14:10."},
		{"The export of every table ran without one error. I'll check back at 14:10. The run is green so far.", "table ran without one error. I'll check", "I'll check back at 14:10."},
		// a question before the promise is an ask, cut or not
		{"Was it the staging rule that you meant by the note in the plan? I'll check back on it at 14:10.", "ing rule that you meant", ""},
	} {
		words := byEnds(long + c.end)
		if !strings.HasPrefix(tail(words), c.tail) || !strings.Contains(words, "… ") {
			t.Fatalf("setup: the kept end does not begin %q: %q", c.tail, tail(words))
		}
		if got := Promise(words); got != c.quote {
			t.Errorf("Promise(cut %q) = %q, want %q", c.end, got, c.quote)
		}
		// the words keep the last two sentences whole now: an offer is seen as one, and a promise is read whole
		whole := c.end
		if i := strings.Index(c.end, ". I'll check back"); i >= 0 {
			whole = "I'll check back at 14:10."
		}
		if got, want := Promise(Words(long+c.end)), map[bool]string{true: whole}[!AsksOwner(c.end)]; got != want {
			t.Errorf("Promise(whole %q) = %q, want %q", c.end, got, want)
		}
	}
	// words whose kept end is a harness's trailer are read from their start, and nothing of that is cut
	if got, want := Promise("I'll check back at 14:10.… </rollout_ids>"), "I'll check back at 14:10."; got != want {
		t.Errorf("a kept start = %q, want %q", got, want)
	}
	// a kept end that opens with a question's mark: the sentence before asked
	if got := Promise("The report lists every table.… ? I'll check back at 14:10."); got != "" {
		t.Errorf("a kept end after a question = %q", got)
	}
}

// A sentence ends at a mark a space follows, when what comes next does not go on in small letters.
func TestSentencesEndAtAMarkAndASpace(t *testing.T) {
	for text, want := range map[string][]string{
		"Done. Next step.":                         {"Done.", "Next step."},
		"See v2.1 of docs/plan.md, then run it.":   {"See v2.1 of docs/plan.md, then run it."},
		"Two shapes (e.g. a table) work. Pick one": {"Two shapes (e.g. a table) work.", "Pick one"},
		"He said \"stop.\" Then it ran!  Really?":  {"He said \"stop.\"", "Then it ran!", "Really?"},
		"**Landed.** `x.go` is in.":                {"**Landed.**", "`x.go` is in."},
		"The run is green. go test passes now.":    {"The run is green. go test passes now."},
		"  ":                                       nil,
	} {
		if got := sentences(text); !slices.Equal(got, want) {
			t.Errorf("sentences(%q) = %q, want %q", text, got, want)
		}
	}
	if got, want := pieces("The run is green. go test passes (e.g. on v2.1) now."), []string{"The run is green.", "go test passes (e.g.", "on v2.1) now."}; !slices.Equal(got, want) {
		t.Errorf("pieces = %q, want %q", got, want)
	}
	// an ask keeps the sentence it hangs on: a file's name after the question does not push it out of the last two
	if got := Asked("Should I also update README.md? I've left config.yaml as is. The rest is in."); got != "" {
		t.Errorf("an ask three sentences back = %q", got)
	}
	if got, want := Asked("The rest is in. Should I also update README.md? I've left config.yaml as is."), "Should I also update README.md?"; got != want {
		t.Errorf("Asked = %q, want %q", got, want)
	}
	if got, want := Question("The rest is in. Should I also update README.md? I've left config.yaml as is."), "Should I also update README.md?"; got != want {
		t.Errorf("Question = %q, want %q", got, want)
	}
}

func TestQuestionIsWhatTheTurnAsksTheOwner(t *testing.T) {
	q := "Should I drop those two rows from the import, or keep them and fill the owner in from the old table?"
	long := strings.Repeat("The table holds the rows we loaded today, and every column matches the schema. ", 4) +
		"Two of the rows have no owner, which the old importer allowed. " + q
	if got := Question(long); got != q {
		t.Fatalf("question %q", got)
	}
	if got := Asked(byEnds(long)); got == "" || len(got) >= len(q) {
		t.Fatalf("words cut at a byte count keep only the end of it: %q", got)
	}
	if got := Asked(Words(long)); got != q {
		t.Fatalf("the words keep the last two sentences whole: %q", got)
	}
	if got := Question("Merged and tagged. Nothing else is left."); got != "" {
		t.Fatalf("no question: %q", got)
	}
}

// byEnds is a text as the store kept it before it kept whole sentences, and as it keeps one whose end does not
// fit: its first 160 and last 80 bytes.
func byEnds(s string) string {
	if len(s) <= headChars+tailChars {
		return fold(s)
	}
	return fold(prefix(s, headChars)) + "… " + fold(suffix(s, tailChars))
}

func TestWordsKeepWhatTheStoreKeeps(t *testing.T) {
	// an end with no sentence that fits is the last bytes
	long := strings.Repeat("start ", 40) + strings.Repeat("middle ", 60) + "and the end, I'll report back."
	w := Words(long)
	if !strings.HasPrefix(w, "start start") || !strings.HasSuffix(w, "I'll report back.") || !strings.Contains(w, "… ") ||
		len(w) > headChars+tailChars+8 || w != byEnds(long) {
		t.Fatalf("words %q", w)
	}
	// the last two sentences are kept whole, from the mark that closed the one before them
	two := strings.Repeat("The report lists every table and its row count. ", 8) + "The import ran on the staging copy and wrote nothing to the live tables. If you approve it, I'll build the checklist page."
	if w := Words(two); !strings.HasSuffix(w, "… . The import ran on the staging copy and wrote nothing to the live tables. If you approve it, I'll build the checklist page.") {
		t.Fatalf("the last two sentences: %q", w)
	}
	// when the two do not fit, the last one; and a text whose start and end meet is kept whole
	one := strings.Repeat("The report lists every table and its row count. ", 8) + strings.Repeat("the import ran on the staging copy and ", 12) + "wrote nothing. I'll check back at 14:10."
	if w := Words(one); !strings.HasSuffix(w, "… . I'll check back at 14:10.") && !strings.HasSuffix(w, "wrote nothing. I'll check back at 14:10.") || len(tail(w)) < tailChars-8 {
		t.Fatalf("the last sentence: %q", w)
	}
	met := strings.Repeat("The report lists every table. ", 5) + "The import ran on the staging copy. I'll check back at 14:10."
	if w := Words(met); strings.Contains(w, "… ") || w != met {
		t.Fatalf("a text whose ends meet: %q", w)
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

// The rules read a live session's last words as the store keeps an agent's text: the two cuts are one.
func TestWordsAreTheStoresSummary(t *testing.T) {
	start := strings.Repeat("The report lists every table and its row count. ", 8)
	for _, text := range []string{
		"short  text\nhere",
		start + "The import ran on the staging copy. If you approve it, I'll build the checklist page.",
		start + "Plan v2.1 is in docs/plan.md now. Should I go on?\n\nI'll check config.yaml at 14:10.  ",
		start + strings.Repeat("the import ran on the staging copy and ", 14) + "wrote nothing. I'll check back at 14:10.",
		start + strings.Repeat("no sentence ends here and ", 40) + "it goes on",
		strings.Repeat("The report lists every table. ", 5) + "The import ran on the staging copy. I'll check back at 14:10.",
		strings.Repeat(start, 12) + "The import ran. I'll check back at 14:10.",
		strings.Repeat("é", 400) + ". The import ran. I'll check back.",
	} {
		if got, want := Words(text), source.AgentSummary(text); got != want {
			t.Errorf("Words = %q, the store keeps %q", got, want)
		}
	}
}
