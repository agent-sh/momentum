package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func decodeGrade(raw json.RawMessage) (Grade, error) {
	object, err := exactObject(raw, "verdict", "facts", "reason")
	if err != nil {
		return Grade{}, err
	}
	var facts []json.RawMessage
	if err := json.Unmarshal(object["facts"], &facts); err != nil || facts == nil {
		return Grade{}, fmt.Errorf("facts must be an array")
	}
	for _, fact := range facts {
		if _, err := exactObject(fact, "fact", "reached"); err != nil {
			return Grade{}, err
		}
	}
	var schema struct {
		Verdict *string `json:"verdict"`
		Facts   []struct {
			Fact    *string `json:"fact"`
			Reached *bool   `json:"reached"`
		} `json:"facts"`
		Reason *string `json:"reason"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&schema); err != nil {
		return Grade{}, err
	}
	if schema.Verdict == nil || schema.Reason == nil || schema.Facts == nil || !validVerdict(*schema.Verdict) {
		return Grade{}, fmt.Errorf("grade needs verdict, facts and reason of the schema's types")
	}
	for _, fact := range schema.Facts {
		if fact.Fact == nil || fact.Reached == nil {
			return Grade{}, fmt.Errorf("each fact needs a string fact and a boolean reached")
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Grade{}, fmt.Errorf("grade has trailing data")
	}
	var grade Grade
	if err := json.Unmarshal(raw, &grade); err != nil {
		return Grade{}, err
	}
	return grade, nil
}

// exactObject rejects duplicate, differently cased, missing and extra names.
// A map decode alone would lose repeated members before checking them.
func exactObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	return parseObject(raw, true, names...)
}

func parseObject(raw []byte, required bool, names ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("expected an object")
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok || !allowed[name] {
			return nil, fmt.Errorf("unexpected member name")
		}
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("repeated member %s", name)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("object has trailing data")
	}
	if required {
		for _, name := range names {
			if _, exists := out[name]; !exists {
				return nil, fmt.Errorf("missing member %s", name)
			}
		}
	}
	return out, nil
}
