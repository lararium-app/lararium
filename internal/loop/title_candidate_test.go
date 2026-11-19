package loop

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
)

func TestTitleCandidate(t *testing.T) {
	tests := []struct {
		name, text, want string
	}{
		{
			name: "plain message passes through",
			text: "anything important in these?",
			want: "anything important in these?",
		},
		{
			name: "client-form block yields the question",
			text: contextBlockHeader + "\n- Lunch at noon (com.example.calendar)\n- 3 new messages (com.example.chat)\n\nanything important in these?",
			want: "anything important in these?",
		},
		{
			name: "context-only message yields no title",
			text: contextBlockHeader + "\n- Lunch at noon (com.example.calendar)",
			want: "",
		},
		{
			name: "header alone yields no title",
			text: contextBlockHeader,
			want: "",
		},
		{
			name: "block marker mid-message is left alone",
			text: "btw " + contextBlockHeader + "\n\nweird?",
			want: "btw " + contextBlockHeader + "\n\nweird?",
		},
		{
			name: "question with internal blank lines keeps all of it",
			text: contextBlockHeader + "\n- ping (pkg)\n\nfirst para\n\nsecond para",
			want: "first para\n\nsecond para",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := titleCandidate(tc.text); got != tc.want {
				t.Fatalf("titleCandidate = %q, want %q", got, tc.want)
			}
		})
	}
}

func readSessionTitle(t *testing.T, sessDir string) *string {
	t.Helper()
	data, err := os.ReadFile(sessDir + "/session.json")
	if err != nil {
		t.Fatal(err)
	}
	var h penatus.SessionHeader
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	return h.Title
}

func TestBackfilledTitleSkipsContextBlock(t *testing.T) {
	root := t.TempDir()
	log, err := penatus.CreateSession(root, "", "main")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	msg := contextBlockHeader + "\n- Lunch at noon (com.example.calendar)\n\nwhat should I pack?"
	wrote, err := penatus.SetTitleIfEmpty(log.Dir(), titleCandidate(msg))
	if err != nil || !wrote {
		t.Fatalf("back-fill: wrote=%v err=%v", wrote, err)
	}
	title := readSessionTitle(t, log.Dir())
	if title == nil || *title != "what should I pack?" {
		t.Fatalf("title = %v, want %q", title, "what should I pack?")
	}
}

func TestContextOnlyMessageLeavesTitleNil(t *testing.T) {
	root := t.TempDir()
	log, err := penatus.CreateSession(root, "", "main")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	msg := contextBlockHeader + "\n- Lunch at noon (com.example.calendar)"
	wrote, err := penatus.SetTitleIfEmpty(log.Dir(), titleCandidate(msg))
	if wrote || err != nil {
		t.Fatalf("context-only message must not write a title: wrote=%v err=%v", wrote, err)
	}
	if title := readSessionTitle(t, log.Dir()); title != nil {
		t.Fatalf("title = %q, want nil", *title)
	}
}
