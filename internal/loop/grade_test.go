package loop

import (
	"encoding/json"
	"testing"
)

func TestGradeRequiresTheCompleteSchema(t *testing.T) {
	for _, raw := range []string{
		`{"verdict":"reached"}`,
		`{"verdict":"reached","facts":[]}`,
		`{"verdict":"reached","reason":"r"}`,
		`{"verdict":"reached","facts":null,"reason":"r"}`,
		`{"verdict":"reached","facts":[],"reason":null}`,
		`{"verdict":"reached","facts":[],"reason":9}`,
		`{"verdict":"other","facts":[],"reason":"r"}`,
		`{"verdict":"reached","facts":[],"reason":"r","extra":true}`,
		`{"verdict":"reached","facts":[{"fact":"f"}],"reason":"r"}`,
		`{"verdict":"reached","facts":[{"reached":true}],"reason":"r"}`,
		`{"verdict":"reached","facts":[{"fact":null,"reached":true}],"reason":"r"}`,
		`{"verdict":"reached","facts":[{"fact":"f","reached":null}],"reason":"r"}`,
		`{"verdict":"reached","facts":[{"fact":"f","reached":"yes"}],"reason":"r"}`,
		`{"verdict":"reached","facts":[{"fact":"f","reached":true,"extra":1}],"reason":"r"}`,
		`{"verdict":"reached","facts":[],"reason":"r"} {}`,
	} {
		if grade, err := decodeGrade(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted malformed grade %s: %+v", raw, grade)
		}
	}
	for _, raw := range []string{
		`{"verdict":"reached","facts":[],"reason":""}`,
		`{"verdict":"partial","facts":[{"fact":"marker-7f3a","reached":false}],"reason":"r"}`,
	} {
		if _, err := decodeGrade(json.RawMessage(raw)); err != nil {
			t.Errorf("refused valid grade %s: %v", raw, err)
		}
	}
}
