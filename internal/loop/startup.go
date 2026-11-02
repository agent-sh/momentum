package loop

import (
	"bufio"
	"encoding/json"
	"io"
)

// sandboxStarted drains bubblewrap's status stream. Unknown objects are ignored.
func sandboxStarted(r io.Reader) bool {
	started := false
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		var status struct {
			Child int `json:"child-pid"`
		}
		if json.Unmarshal(scanner.Bytes(), &status) == nil && status.Child > 0 {
			started = true
		}
	}
	return started
}
