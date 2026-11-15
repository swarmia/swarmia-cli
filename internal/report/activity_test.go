package report

import (
	"testing"
	"time"
)

func TestLastActivity(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	min := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	ptr := func(t time.Time) *time.Time { return &t }

	tests := []struct {
		name string
		s    Session
		want time.Time // zero: none
	}{
		{"none", Session{StartedAt: t0, EndedAt: min(600)}, time.Time{}},
		{"reply", Session{Turns: []Turn{{At: min(0), Text: "a", EndedAt: ptr(min(5))}}}, min(5)},
		{"unanswered prompt", Session{
			Turns: []Turn{{At: min(0), Text: "a", EndedAt: ptr(min(5))}, {At: min(20), Text: "b"}},
			Tools: []Tool{{At: min(3)}},
		}, min(20)},
		{"interrupted turn counts to its last tool call", Session{
			Turns: []Turn{{At: min(0), Text: "a"}},
			Tools: []Tool{{At: min(4)}, {At: min(9)}},
		}, min(9)},
		{"edit", Session{
			Turns: []Turn{{At: min(0), Text: "a"}},
			Edits: []Edit{{At: min(7), Path: "x.go"}},
		}, min(7)},
		{"automatic turn and its work don't count", Session{
			Turns: []Turn{
				{At: min(0), Text: "a", EndedAt: ptr(min(5))},
				{At: min(300), Automatic: true, EndedAt: ptr(min(310))},
			},
			Tools: []Tool{{At: min(2)}, {At: min(305)}},
			Edits: []Edit{{At: min(306), Path: "x.go"}},
		}, min(5)},
		{"a prompt after an automatic turn counts", Session{
			Turns: []Turn{
				{At: min(0), Automatic: true, EndedAt: ptr(min(5))},
				{At: min(30), Text: "b", EndedAt: ptr(min(40))},
			},
			Tools: []Tool{{At: min(2)}, {At: min(35)}},
		}, min(40)},
		{"tool calls before any turn count", Session{Tools: []Tool{{At: min(1)}}}, min(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.s.LastActivity()
			if ok != !tt.want.IsZero() || !got.Equal(tt.want) {
				t.Errorf("LastActivity() = %v, %v; want %v", got, ok, tt.want)
			}
		})
	}
}
