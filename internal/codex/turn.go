package codex

import (
	"encoding/json"
	"strings"
	"unicode"
)

// Turn is what the JSONL of one `codex exec --json` run said. It is the parse
// both `parley doctor --live` and `parley table new` (and `parley build new`)
// read a run with: the first proves a thread id is the one the model sees, the
// others start a seat's or a peer's session and record its id.
type Turn struct {
	// ThreadID is the thread.started event's thread id, the first one if there
	// were several. It is whatever the line carried: the caller validates it.
	ThreadID string
	// Said holds the command outputs and the texts of the run's completed items,
	// the places a thread id is looked for; two entries per item, either of which
	// may be empty.
	Said []string
	// Answer is the text of the last completed agent_message item, the model's
	// final message of the run.
	Answer string
	// Events lists the lines by event type, for a report of a run that did not
	// look like the one expected; a line that is not JSON is "(not JSON)". The
	// types are printed, so each has had its control characters taken out.
	Events []string
	// Failure is the type and message of the first error or failed turn the
	// stream had, made safe to print on one line. A turn that has one failed,
	// whatever codex exited with.
	Failure string
	// Completed is whether a turn.completed event was seen. A run that exits 0
	// without one is not shown to have finished a turn.
	Completed bool
	// Truncated is set by a caller that read the run's output through a bound and
	// found it cut off: ParseTurn is handed a string and cannot know. Such a run
	// is not shown to have completed even if a turn.completed is in what was kept.
	Truncated bool
}

// PlainLine is text taken from codex's output made safe to print on one line of
// a terminal: every control character is gone, a tab, a line break or a Unicode
// line or paragraph separator becomes a space, and the ends are trimmed. Event
// types, error messages and stderr are a child's text, and an escape sequence in
// them would be the terminal's to act on.
func PlainLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// event is the part of a `codex exec --json` line the parse reads. These are the
// lines codex 0.161.0 printed for a live turn (internal/doctor/testdata has the
// whole run):
//
//	{"type":"thread.started","thread_id":"<uuid>"}
//	{"type":"turn.started"}
//	{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"/bin/bash -lc 'printenv CODEX_THREAD_ID'","aggregated_output":"","exit_code":null,"status":"in_progress"}}
//	{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"…","aggregated_output":"<uuid>\n","exit_code":0,"status":"completed"}}
//	{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"<uuid>"}}
//	{"type":"turn.completed","usage":{"input_tokens":…,"cached_input_tokens":…,"cache_write_input_tokens":…,"output_tokens":…,"reasoning_output_tokens":…}}
//
// A failed turn has not been observed here. error and turn.failed are read for a
// message in the shapes {"type":"error","message":"…"} and
// {"type":"turn.failed","error":{"message":"…"}}, which are assumed and only
// ever add detail to a failure that the exit status or the missing id already is.
type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		AggregatedOutput string `json:"aggregated_output"`
	} `json:"item"`
}

// ParseTurn reads the stdout of a `codex exec --json` run. It never fails: a
// line it cannot read is recorded as such and skipped, and what the run did not
// say is the empty value, so a partial run — a timeout, a kill — still yields
// the thread id it had announced, which is what a caller must delete. Completed
// says whether a turn.completed event was seen.
func ParseTurn(out string) Turn {
	var t Turn
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Events = append(t.Events, "(not JSON)")
			continue
		}
		t.Events = append(t.Events, PlainLine(ev.Type))
		switch ev.Type {
		case "thread.started":
			if t.ThreadID == "" {
				t.ThreadID = ev.ThreadID
			}
		case "item.completed":
			if ev.Item != nil {
				t.Said = append(t.Said, ev.Item.AggregatedOutput, ev.Item.Text)
				if ev.Item.Type == "agent_message" {
					t.Answer = ev.Item.Text
				}
			}
		case "turn.completed":
			t.Completed = true
		case "error", "turn.failed":
			msg := ev.Message
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			if t.Failure == "" {
				t.Failure = PlainLine(ev.Type + ": " + msg)
			}
		}
	}
	return t
}
