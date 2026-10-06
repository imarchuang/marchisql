package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// MatchWhere reports whether a version's fields satisfy a single equality
// predicate of the form `field=value`.
//
// value is a JSON literal when it parses as one (true, false, null, number);
// otherwise it is a string. `on_call=true` matches `{"on_call":true}`. An
// empty predicate matches everything. A missing field does not match.
func MatchWhere(fields json.RawMessage, where string) (bool, error) {
	if where == "" {
		return true, nil
	}
	key, lit, ok := strings.Cut(where, "=")
	if !ok || key == "" || lit == "" {
		return false, fmt.Errorf("where must be field=value, got %q", where)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(fields, &obj); err != nil {
		return false, nil
	}
	got, ok := obj[key]
	if !ok {
		return false, nil
	}
	want, err := whereLiteral(lit)
	if err != nil {
		return false, err
	}
	return bytes.Equal(bytes.TrimSpace(got), want), nil
}

func whereLiteral(lit string) (json.RawMessage, error) {
	switch lit {
	case "true", "false", "null":
		return json.RawMessage(lit), nil
	}
	if _, err := strconv.ParseInt(lit, 10, 64); err == nil {
		return json.RawMessage(lit), nil
	}
	if _, err := strconv.ParseFloat(lit, 64); err == nil {
		return json.RawMessage(lit), nil
	}
	b, err := json.Marshal(lit)
	if err != nil {
		return nil, err
	}
	return b, nil
}
