package scorecard

import (
	"bufio"
	"bytes"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// A restatement is an owner message that says again what a curated fact of tacit's already holds: the owner had to
// say it because the agent did not know it, or did not use it. Finding one is a text match, tuned for precision: a
// message counts only when it covers most of a fact's distinctive words, the fact is older than the message, and the
// fact was not made from that session. A missed restatement costs the scorecard a little recall; a false one would
// count against tacit for what it never held.

// Fact is a curated fact as the matcher reads it.
type Fact struct {
	Name, Text string
	Created    time.Time // its day; zero when it names none
	From       string    // the session its owner's words came from, when it names one
	terms      []string
}

// uncurated are the store's top folders whose notes are not curated facts: the agents' inbox, the reviewer's
// queue, derived views and keys.
var uncurated = map[string]bool{"inbox": true, "review": true, "derived": true, "keys": true}

// LoadFacts reads the curated facts of a tacit store: every Markdown file with a frontmatter name outside the
// uncurated folders, its description and owner's words the text matched.
func LoadFacts(dir string) ([]Fact, error) {
	var out []Fact
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(name, ".") || filepath.Dir(p) == dir && uncurated[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(name, ".md") || name == "_index.md" || name == "MEMORY.md" {
			return nil
		}
		if f, ok := readFact(p); ok {
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// readFact reads a fact's frontmatter: name, description, created, and its source's owner_words and
// owner_words_from. A folded or literal block value takes its indented lines.
func readFact(path string) (Fact, bool) {
	b, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(b, []byte("---")) {
		return Fact{}, false
	}
	fields := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	sc.Scan() // ---
	key, keyIndent, block := "", 0, false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "---" {
			break
		}
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if key != "" && trimmed != "" && indent > keyIndent && (block || !strings.Contains(trimmed, ": ")) {
			fields[key] = strings.TrimSpace(fields[key] + " " + trimmed)
			continue
		}
		key, block = "", false
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		switch k {
		case "name", "description", "created", "owner_words", "owner_words_from", "session", "retired", "superseded_by":
			if _, dup := fields[k]; dup {
				continue
			}
			v = strings.TrimSpace(v)
			key, keyIndent = k, indent
			if v == ">" || v == ">-" || v == "|" || v == "|-" {
				v, block = "", true
			}
			fields[k] = unquote(v)
		}
	}
	f := Fact{Name: fields["name"], From: strings.TrimSpace(fields["owner_words_from"] + " " + fields["session"])}
	if r := strings.ToLower(fields["retired"]); f.Name == "" || r != "" && r != "false" && r != "no" || fields["superseded_by"] != "" {
		return Fact{}, false // a fact tacit no longer holds is none the owner could restate
	}
	f.Text = strings.TrimSpace(fields["description"] + " " + fields["owner_words"])
	if t, err := time.Parse("2006-01-02", firstN(fields["created"], 10)); err == nil {
		f.Created = t
	}
	return f, f.Text != ""
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
	}
	return v
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// stop are words that say nothing of what a fact holds.
var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for are but not you all any can had her was one our out has his how its
	may new now old see two who did get got let put say she too use way also been from have into just more most much must
	only over said same some such than that them then they this were what when where which while will with would your
	about after again before being both could does doing done down each even ever every here just like make many might
	never other really should since still their there these those though through under until very want well what whom why
	yes yet ive dont didnt doesnt cant wont isnt im its it's lets thats theres whats u2019 use used using uses need needs
	needed thing things something anything nothing ok okay please thanks also because way much why just know think
	owner agent agents said says say told tell asked ask fact facts note notes rule rules`) {
		stop[w] = true
	}
}

// Terms are the words of a text the matcher reads: lower case letters and digits, at least three long, not a stop
// word, a plural's s taken off.
func Terms(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < 3 || stop[w] {
			continue
		}
		if len(w) > 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// Matcher finds the curated fact an owner message restates.
type Matcher struct {
	facts []Fact
	idf   map[string]float64
	index map[string][]int // a distinctive term's facts
}

// The matcher's bounds. A fact needs minFactTerms distinct words to be matched at all (a short fact is matched by
// chance); a message restates it when they share minShared of its words and those carry minCover of its words'
// weight, and are minMessage of the message's own weight (a long message covers any fact a little). A term in more
// than indexShare of the facts finds no candidate: it is too common to point at one.
const (
	minFactTerms = 5
	minShared    = 4
	minCover     = 0.6
	minMessage   = 0.25
	indexShare   = 0.005
	maxMessage   = 2000 // characters; a longer message is a paste, not a restatement
)

// NewMatcher indexes the facts.
func NewMatcher(facts []Fact) *Matcher {
	m := &Matcher{idf: map[string]float64{}, index: map[string][]int{}}
	df := map[string]int{}
	for _, f := range facts {
		f.terms = Terms(f.Name + " " + f.Text)
		if len(f.terms) < minFactTerms {
			continue
		}
		m.facts = append(m.facts, f)
		for _, t := range f.terms {
			df[t]++
		}
	}
	n := float64(len(m.facts))
	limit := max(3, int(indexShare*n))
	for t, d := range df {
		m.idf[t] = math.Log(1 + n/float64(d))
	}
	for i, f := range m.facts {
		for _, t := range f.terms {
			if df[t] <= limit {
				m.index[t] = append(m.index[t], i)
			}
		}
	}
	return m
}

// Facts is how many facts the matcher holds.
func (m *Matcher) Facts() int { return len(m.facts) }

// Match is the fact a message of a session at a time restates best, and its cover; ok is false when it restates
// none. A fact made after the message's day, or from its session, is not one it could restate.
func (m *Matcher) Match(text, session string, at time.Time) (Fact, float64, bool) {
	if len(text) > maxMessage || pasted(text) {
		return Fact{}, 0, false
	}
	msg := Terms(text)
	in := map[string]bool{}
	msgWeight := 0.0
	for _, t := range msg {
		in[t] = true
		msgWeight += m.idf[t]
	}
	cands := map[int]bool{}
	for _, t := range msg {
		for _, i := range m.index[t] {
			cands[i] = true
		}
	}
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	best, bestCover := -1, 0.0
	for i := range cands {
		f := m.facts[i]
		if f.Created.IsZero() || !f.Created.Before(day) || session != "" && strings.Contains(f.From, session) {
			continue
		}
		shared, weight, all := 0, 0.0, 0.0
		for _, t := range f.terms {
			all += m.idf[t]
			if in[t] {
				shared++
				weight += m.idf[t]
			}
		}
		if shared < minShared || all == 0 || msgWeight == 0 {
			continue
		}
		cover := weight / all
		if cover < minCover || weight/msgWeight < minMessage {
			continue
		}
		if cover > bestCover || cover == bestCover && best >= 0 && f.Name < m.facts[best].Name {
			best, bestCover = i, cover
		}
	}
	if best < 0 {
		return Fact{}, 0, false
	}
	return m.facts[best], bestCover, true
}

// pasted says a message reads as something pasted rather than said: a list, a table, a link, a path or a heading
// first, a terminal's box drawing, or more other characters than letters.
func pasted(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range []string{"-", "*", "|", "[", "#", "/", "~/", "<", "{", "http://", "https://"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	letters, all := 0, 0
	for _, r := range t {
		if r >= 0x2500 && r <= 0x257f {
			return true
		}
		if !unicode.IsSpace(r) {
			all++
			if unicode.IsLetter(r) {
				letters++
			}
		}
	}
	return all == 0 || 2*letters < all
}
