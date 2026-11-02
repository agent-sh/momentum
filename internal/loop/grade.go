package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func decodeGrade(raw json.RawMessage) (Grade, error) {
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
