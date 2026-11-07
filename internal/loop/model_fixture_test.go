package loop

import (
	"encoding/json"
	"fmt"
	"strings"
)

// fixtureResult posts each synthetic call and prints the harness's result line.
func fixtureResult(result string) string {
	var r struct {
		Turns   int             `json:"num_turns"`
		Text    string          `json:"result"`
		Report  json.RawMessage `json:"structured_output"`
		Output  int             `json:"fixture_output_tokens"`
		NoUsage bool            `json:"fixture_no_usage"`
	}
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		panic(err)
	}
	maximum := 1000
	if r.Output > 0 {
		maximum = r.Output
	}
	// the report travels as text: a report is JSON data that may hold a "source" key, which the relay refuses outside
	// a tool input
	report := string(r.Report)
	if report == "" {
		report = "null"
	}
	body, _ := json.Marshal(map[string]any{"max_tokens": maximum, "messages": []any{map[string]string{"role": "user", "content": "marker-7f3a"}}, "fixture_report": report, "fixture_refusal": strings.Contains(r.Text, "safeguards"), "fixture_output_tokens": r.Output, "fixture_no_usage": r.NoUsage})
	return fmt.Sprintf(`python3 - %s %d <<'PYFIXTURE'
import sys, os, urllib.request, time
body=sys.argv[1].encode()
url=os.environ['ANTHROPIC_BEDROCK_BASE_URL']+'/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream'
for i in range(int(sys.argv[2])):
    for retry in range(40):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, data=body, headers={'Content-Type':'application/json'}),timeout=5) as response:
                response.read()
            break
        except urllib.error.HTTPError:
            sys.exit(1)
        except urllib.error.URLError:
            if retry == 39: sys.exit(1)
            time.sleep(.025)
PYFIXTURE
printf '%%s\n' %s`, ShellQuote(string(body)), max(1, r.Turns), ShellQuote(result))
}
func fixtureCharge(turns int) float64 {
	var sum float64
	for i := 0; i < turns; i++ {
		sum += .00204
	}
	return sum
}
func fixtureTokens(turns int) int64 { return 110 * int64(turns) }
