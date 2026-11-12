package sessions

import (
	"regexp"
	"time"
)

// Error classes: what an API error that ended a turn says about trying again.
const (
	Transient = "transient" // the provider or the network failed for a while: try again later
	Login     = "login"     // the login expired: only its owner can log in again
	Limit     = "limit"     // a usage limit: it resets at a time the error states
	Org       = "org"       // the organization turned the access off
)

var (
	// Never tried again: a refusal, a request too long to send, a misuse.
	reNoRetry = regexp.MustCompile(`(?i)safeguards flagged|safety classifier|usage policy|prompt is too long|too long for|context (window|length) (exceeded|is full)|invalid[_ ]request|credit balance|capture only|tool use concurrency`)
	reOrg     = regexp.MustCompile(`(?i)organization has disabled|disabled (claude )?subscription access|ask your admin to enable`)
	reLogin   = regexp.MustCompile(`(?i)login expired|not logged in|please run /login|oauth token (has )?(expired|revoked)|authentication_error|invalid api key|401 unauthorized`)
	reLimit   = regexp.MustCompile(`(?i)hit your (\w+[- ])?limit|(usage|session|weekly|daily|\d+-hour) limit (reached|hit)|limit reached\s*[·∙•|]|out of (extra )?usage|^rate_limit: `)
	reTrans   = regexp.MustCompile(`(?i)api error: (5\d\d|429|529)\b|bedrock is unable to process|can't reach the api server|server error mid-response|unexpected error during processing|connection (refused|error|reset|closed)|request timed out|timed out|overloaded|econnreset|eai_again|enotfound|fetch failed|socket hang up|network error|throttl|too many requests|service unavailable|internal server error|bad gateway|gateway time-?out`)
)

// ErrorClass says what an API error that ended a turn is, or "" for one never tried again blindly.
func ErrorClass(text string) string {
	switch {
	case text == "", reNoRetry.MatchString(text):
		return ""
	case reOrg.MatchString(text):
		return Org
	case reLogin.MatchString(text):
		return Login
	case reLimit.MatchString(text):
		return Limit
	case reTrans.MatchString(text):
		return Transient
	}
	return ""
}

// accessKinds are the kinds of API error Claude Code names for a wall it sends no request through: a usage limit, a
// login that failed, an organization that turned access off, billing.
var accessKinds = map[string]bool{"rate_limit": true, "authentication_failed": true, "oauth_org_not_allowed": true, "billing_error": true}

// HeldBy reports that an API error that ended a turn (its kind, as Claude Code names it, its words, its time) stops
// access, so Claude Code sends no request for a task's notification that comes at at, until the reset a limit
// states when it states one. The kind must be an access kind and the words must not read as transient (a throttle
// Claude Code names rate_limit is one to try again); with no kind, as the store keeps an error, the words must read
// as an access error (a login, a usage limit, an organization's block). The live tracker and the replay both read
// it here.
func HeldBy(kind, words string, errAt, at time.Time) bool {
	class := ErrorClass(words)
	switch {
	case kind != "" && (!accessKinds[kind] || class == Transient):
		return false
	case kind == "" && class != Login && class != Limit && class != Org:
		return false
	}
	reset, ok := ResetAt(words, errAt)
	return !ok || at.Before(reset)
}
