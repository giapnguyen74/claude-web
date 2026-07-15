package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTailStart(t *testing.T) {
	mk := func(sizes ...int) []json.RawMessage {
		out := make([]json.RawMessage, len(sizes))
		for i, s := range sizes {
			out[i] = json.RawMessage(strings.Repeat("x", s))
		}
		return out
	}
	cases := []struct {
		name           string
		events         []json.RawMessage
		maxBytes, maxN int
		want           int
	}{
		{"empty", mk(), 100, 10, 0},
		{"count cap", mk(10, 10, 10, 10, 10), 1 << 20, 3, 2},
		{"byte cap", mk(100, 100, 100, 100, 100), 250, 1000, 2},
		{"oversized last always shown", mk(5, 5, 999), 100, 1000, 2},
		{"everything fits", mk(10, 10, 10), 1 << 20, 1000, 0},
	}
	for _, c := range cases {
		if got := tailStart(c.events, c.maxBytes, c.maxN); got != c.want {
			t.Errorf("%s: tailStart = %d; want %d", c.name, got, c.want)
		}
	}
}

func TestOriginValidation(t *testing.T) {
	tests := []struct {
		origin         string
		allowedOrigins []string
		expected       bool
	}{
		// Local loopback is always allowed
		{"", nil, true},
		{"http://localhost", nil, true},
		{"http://localhost:4000", nil, true},
		{"http://127.0.0.1", nil, true},
		{"http://127.0.0.1:8080", nil, true},
		{"http://[::1]", nil, true},
		{"http://[::1]:9000", nil, true},

		// Hostname prefix attacks must be rejected
		{"http://localhost.evil.com", nil, false},
		{"http://127.0.0.1.attacker.com", nil, false},
		{"http://evil-localhost.example.com:8080", nil, false},

		// Custom remote host/same-origin remote host must be rejected by default
		{"http://xxxx:4000", nil, false},
		{"http://192.168.1.100:4000", nil, false},

		// Custom remote host allowed via explicit matches
		{"http://xxxx:4000", []string{"xxxx:4000"}, true},
		{"http://xxxx:4000", []string{"xxxx"}, true},
		{"http://xxxx:4000", []string{"http://xxxx:4000"}, true},
		{"http://xxxx:4000", []string{"yyyy"}, false},
		{"http://192.168.1.100:4000", []string{"192.168.1.100"}, true},
	}

	for _, tc := range tests {
		got := checkOrigin(tc.origin, tc.allowedOrigins)
		if got != tc.expected {
			t.Errorf("checkOrigin(%q, %v) = %v; want %v", tc.origin, tc.allowedOrigins, got, tc.expected)
		}
	}
}

func TestIsLoopbackHostPort(t *testing.T) {
	tests := []struct {
		host     string
		expected bool
	}{
		// Loopback — allowed in passwordless mode.
		{"", true}, // non-browser clients that omit Host
		{"localhost", true},
		{"localhost:4000", true},
		{"127.0.0.1", true},
		{"127.0.0.1:4000", true},
		{"127.0.0.2:8080", true}, // all of 127.0.0.0/8 is loopback
		{"[::1]:4000", true},
		{"::1", true},
		// Non-loopback — a rebinding attacker's Host, rejected.
		{"evil.com:4000", false},
		{"localhost.evil.com:4000", false},
		{"192.168.1.100:4000", false},
		{"10.0.0.5", false},
	}
	for _, tc := range tests {
		if got := isLoopbackHostPort(tc.host); got != tc.expected {
			t.Errorf("isLoopbackHostPort(%q) = %v; want %v", tc.host, got, tc.expected)
		}
	}
}

func TestTranslateSubmitToClaudeStreamJSON(t *testing.T) {
	submitInput := map[string]any{
		"type": "submit",
		"text": "hello test prompt",
	}

	gotBytes, err := toClaudeInput(submitInput)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(gotBytes, &got); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	if got["type"] != "user" {
		t.Errorf("expected type 'user', got %q", got["type"])
	}

	msg, ok := got["message"].(map[string]any)
	if !ok {
		t.Fatalf("missing or invalid 'message' key")
	}

	if msg["role"] != "user" {
		t.Errorf("expected role 'user', got %q", msg["role"])
	}

	content, ok := msg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("missing or empty 'content' array")
	}

	firstBlock, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content block is not an object")
	}

	if firstBlock["type"] != "text" || firstBlock["text"] != "hello test prompt" {
		t.Errorf("unexpected content block: %+v", firstBlock)
	}
}
