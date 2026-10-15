package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

const realThread = "01a1218f-e2ce-7173-b121-e4588840f47a"

// TestParseTurnReadsARealCodexRun: the run in internal/doctor/testdata is what
// codex 0.161.0 printed for a real turn, so the shapes read are the shapes that
// exist: the thread id, the model's final message, every event.
func TestParseTurnReadsARealCodexRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "doctor", "testdata", "codex-0.161.0-exec-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	turn := ParseTurn(string(raw))
	if turn.ThreadID != realThread {
		t.Errorf("thread id %q", turn.ThreadID)
	}
	if turn.Answer != realThread {
		t.Errorf("answer %q, want the agent_message text %q", turn.Answer, realThread)
	}
	if turn.Failure != "" {
		t.Errorf("failure %q", turn.Failure)
	}
	if !turn.Completed {
		t.Error("the real run ends in turn.completed, and was read as not completed")
	}
	want := "thread.started, turn.started, item.started, item.completed, item.completed, turn.completed"
	if got := strings.Join(turn.Events, ", "); got != want {
		t.Errorf("events %q, want %q", got, want)
	}
	if len(turn.Said) != 4 {
		t.Errorf("said %q, want an output and a text for each of the two completed items", turn.Said)
	}
}

// TestParseTurnReportsWhetherATurnCompleted: only a turn.completed event shows the
// turn finished. A thread, an answer, even an exit 0 do not.
func TestParseTurnReportsWhetherATurnCompleted(t *testing.T) {
	started := `{"type":"thread.started","thread_id":"` + realThread + `"}`
	answer := `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"ready"}}`
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"the whole run", []string{started, `{"type":"turn.started"}`, answer, `{"type":"turn.completed","usage":{"input_tokens":1}}`}, true},
		{"a turn.completed alone", []string{`{"type":"turn.completed"}`}, true},
		{"an answer and no turn.completed", []string{started, `{"type":"turn.started"}`, answer}, false},
		{"a turn.completed cut off mid-line", []string{started, answer, `{"type":"turn.compl`}, false},
		{"a failed turn", []string{started, `{"type":"turn.failed","error":{"message":"quota"}}`}, false},
		{"turn.completed only as text of an item", []string{started, `{"type":"item.completed","item":{"type":"agent_message","text":"turn.completed"}}`}, false},
		{"nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := ParseTurn(strings.Join(tc.lines, "\n"))
			if turn.Completed != tc.want {
				t.Errorf("Completed = %v, want %v", turn.Completed, tc.want)
			}
			if turn.Truncated {
				t.Error("ParseTurn cannot know the output was cut off, so it never sets Truncated")
			}
		})
	}
}

// TestParseTurnTheAnswerIsTheLastAgentMessage: a command's output is not an
// answer, and a later message replaces an earlier one.
func TestParseTurnTheAnswerIsTheLastAgentMessage(t *testing.T) {
	turn := ParseTurn(strings.Join([]string{
		`{"type":"thread.started","thread_id":"` + realThread + `"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"not an answer"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"second"}}`,
	}, "\n"))
	if turn.Answer != "second" {
		t.Errorf("answer %q, want the last agent message", turn.Answer)
	}
}

// TestParseTurnOfAKilledRunStillHasTheThread: a run cut short has announced the
// thread, and that is the one fact a caller has to delete it by.
func TestParseTurnOfAKilledRunStillHasTheThread(t *testing.T) {
	turn := ParseTurn("not json\n" + `{"type":"thread.started","thread_id":"` + realThread + `"}` + "\n" + `{"type":"turn.started"}` + "\n" + `{"type":"item.comp`)
	if turn.ThreadID != realThread {
		t.Errorf("thread id %q", turn.ThreadID)
	}
	if turn.Answer != "" {
		t.Errorf("answer %q from a run that gave none", turn.Answer)
	}
	if got := strings.Join(turn.Events, ", "); got != "(not JSON), thread.started, turn.started, (not JSON)" {
		t.Errorf("events %q", got)
	}
}

func TestParseTurnReadsFailures(t *testing.T) {
	for out, want := range map[string]string{
		`{"type":"error","message":"no auth"}`:                                              "error: no auth",
		`{"type":"turn.failed","error":{"message":"model gone"}}`:                           "turn.failed: model gone",
		`{"type":"error","message":"first"}` + "\n" + `{"type":"error","message":"second"}`: "error: first",
	} {
		if got := ParseTurn(out).Failure; got != want {
			t.Errorf("failure of %s = %q, want %q", out, got, want)
		}
	}
	if got := ParseTurn("").ThreadID; got != "" {
		t.Errorf("thread id %q of nothing", got)
	}
}

// TestParseTurnStripsControlCharactersFromWhatItPrints: the event types and the
// failure text are a child's, printed on the human's terminal.
func TestParseTurnStripsControlCharactersFromWhatItPrints(t *testing.T) {
	turn := ParseTurn(strings.Join([]string{
		`{"type":"weird\u001b[2Jtype"}`,
		`{"type":"turn.failed","error":{"message":"bad\u001b[31m thing\u0007\nsecond\tline third"}}`,
	}, "\n"))
	if got := strings.Join(turn.Events, ", "); got != "weird[2Jtype, turn.failed" {
		t.Errorf("events %q", got)
	}
	if want := "turn.failed: bad[31m thing second line third"; turn.Failure != want {
		t.Errorf("failure %q, want %q", turn.Failure, want)
	}
	for _, s := range append([]string{turn.Failure}, turn.Events...) {
		if strings.ContainsFunc(s, unicode.IsControl) {
			t.Errorf("%q still holds a control character", s)
		}
	}
}

func TestPlainLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":             "plain",
		"  padded \t":       "padded",
		"a\x1b[31mb\x07c":   "a[31mbc",
		"one\ntwo\r\nthree": "one two  three",
		"line break here":   "line break here",
		"\x00\x01":          "",
		"héllo ✓":           "héllo ✓",
	} {
		if got := PlainLine(in); got != want {
			t.Errorf("PlainLine(%q) = %q, want %q", in, got, want)
		}
	}
}
