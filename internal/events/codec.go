package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Document is the import/export format: {"events": [...]}.
type Document struct {
	Events []Event `json:"events"`
}

// Decode parses events from a {"events": [...]} document or a bare JSON list
// (so the output of GET /api/events can be imported too).
func Decode(data []byte) ([]Event, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("empty document")
	}
	if data[0] == '[' {
		var list []Event
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return list, nil
	}
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if doc.Events == nil {
		return nil, errors.New(`want {"events": [...]} or a list of events`)
	}
	return doc.Events, nil
}

// Encode renders events as an indented {"events": [...]} document.
func Encode(evs []Event) ([]byte, error) {
	if evs == nil {
		evs = []Event{}
	}
	data, err := json.MarshalIndent(Document{Events: evs}, "", "  ")
	return append(data, '\n'), err
}
