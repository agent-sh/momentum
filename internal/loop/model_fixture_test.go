package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
)

// modelResult models both sides of a fixture harness: its requests to the fake
// model and its final stream record. Authentication tests construct them separately.
func modelResult(result string) string {
	return "GORACE=atexit_sleep_ms=0 " + jail.BinDir + "/momentum fixture-model-result " + shellQuote(result)
}

func sendFixtureResult(result string) int {
	var record struct {
		Turns  int             `json:"num_turns"`
		Text   string          `json:"result"`
		Report json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal([]byte(result), &record); err != nil {
		return 1
	}
	client := http.Client{Timeout: 5 * time.Second}
	for i := 0; i < max(1, record.Turns); i++ {
		body, _ := json.Marshal(map[string]any{"max_tokens": 1000,
			"fixture_report": record.Report, "fixture_refusal": strings.Contains(record.Text, "safeguards")})
		resp, err := client.Post(os.Getenv("ANTHROPIC_BEDROCK_BASE_URL")+"/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream",
			"application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return 1
		}
	}
	fmt.Println(result)
	return 0
}
