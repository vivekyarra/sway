package config

import "testing"

func TestModelFor(t *testing.T) {
	cases := []struct{ mode, configured, fallback, want string }{
		{"gemini", "claude-haiku-5-5", "gemini-flash-lite-latest", "gemini-flash-lite-latest"},
		{"gemini", "gemini-2.5-flash", "gemini-flash-lite-latest", "gemini-2.5-flash"},
		{"claude", "claude-opus-5-5", "claude-haiku-5-5", "claude-opus-5-5"},
		{"claude", "gpt-4o-mini", "claude-haiku-5-5", "claude-haiku-5-5"},
		{"openai", "llama-3.3-70b-versatile", "gpt-4o-mini", "llama-3.3-70b-versatile"},
	}
	for _, c := range cases {
		if got := modelFor(c.mode, c.configured, c.fallback); got != c.want {
			t.Errorf("modelFor(%q,%q) = %q, want %q", c.mode, c.configured, got, c.want)
		}
	}
}
