package router

import "testing"

func TestParseModelEntryContextLength(t *testing.T) {
	entry := func(raw string) modelEntry {
		t.Helper()
		e, err := unmarshalModelEntry([]byte(raw))
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return e
	}

	t.Run("openai-compatible host reports context_length", func(t *testing.T) {
		caps := parseModelEntry(entry(`{"id":"m","context_length":262144}`))
		if caps.ContextLength != 262144 {
			t.Errorf("ContextLength = %d, want 262144", caps.ContextLength)
		}
	})

	t.Run("top_provider window wins over top-level", func(t *testing.T) {
		caps := parseModelEntry(entry(`{"id":"m","context_length":1000000,"top_provider":{"context_length":131072}}`))
		if caps.ContextLength != 131072 {
			t.Errorf("ContextLength = %d, want 131072 (serving window)", caps.ContextLength)
		}
	})

	t.Run("llama.cpp trained_tokens still works", func(t *testing.T) {
		caps := parseModelEntry(entry(`{"id":"m","trained_tokens":8192}`))
		if caps.ContextLength != 8192 {
			t.Errorf("ContextLength = %d, want 8192", caps.ContextLength)
		}
	})

	t.Run("zero context_length does not clobber n_ctx", func(t *testing.T) {
		caps := parseModelEntry(entry(`{"id":"m","n_ctx":4096,"context_length":0}`))
		if caps.ContextLength != 4096 {
			t.Errorf("ContextLength = %d, want 4096", caps.ContextLength)
		}
	})
}
