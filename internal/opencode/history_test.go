package opencode

import (
	"context"
	"strings"
	"testing"
	"time"
)

func idle(outcome string) HistoryMessage {
	return HistoryMessage{ID: "msg_i", Kind: KindIdle, Outcome: outcome}
}
func assistant(finish string) HistoryMessage {
	return HistoryMessage{ID: "msg_a", Kind: KindAssistant, Finish: finish}
}
func user() HistoryMessage { return HistoryMessage{ID: "msg_u", Kind: KindUser} }

func other() HistoryMessage { return HistoryMessage{ID: "msg_s", Kind: "skill"} }

func TestJudgeStopped(t *testing.T) {
	for name, tc := range map[string]struct {
		msgs []HistoryMessage // newest first
		want holdState
	}{
		"nothing yet":                                {nil, holdUndecided},
		"interrupted, nothing after":                 {[]HistoryMessage{idle("interrupted"), assistant("error"), user()}, holdStopped},
		"interrupted, a user message after":          {[]HistoryMessage{user(), idle("interrupted"), assistant("error")}, holdNone},
		"interrupted, then a turn that went through": {[]HistoryMessage{idle("succeeded"), assistant("stop"), user(), idle("interrupted")}, holdNone},
		"succeeded":                                  {[]HistoryMessage{idle("succeeded"), assistant("stop"), user()}, holdNone},
		"failed":                                     {[]HistoryMessage{idle("failed"), assistant("error")}, holdNone},
		"neither marker in what was read":            {[]HistoryMessage{assistant("tool-calls"), other(), assistant("tool-calls")}, holdUndecided},
		"other kinds are read past":                  {[]HistoryMessage{other(), other(), other(), other(), other(), idle("interrupted")}, holdStopped},
		"a user message behind other kinds, newer than the marker": {[]HistoryMessage{other(), other(), other(), other(), user(), idle("interrupted")}, holdNone},
	} {
		if got := judgeStopped(tc.msgs); got != tc.want {
			t.Errorf("%s: judgeStopped = %v, want %v", name, got, tc.want)
		}
	}
}

func TestJudgeTurn(t *testing.T) {
	for name, tc := range map[string]struct {
		msgs  []HistoryMessage
		final bool
		want  verdict
		err   string
	}{
		"succeeded after a step that stopped":  {[]HistoryMessage{idle("succeeded"), assistant("stop"), assistant("tool-calls")}, false, over, ""},
		"empty":                                {nil, false, needMore, ""},
		"empty, and no more to read":           {nil, true, notOver, ""},
		"still running":                        {[]HistoryMessage{assistant("tool-calls"), user()}, false, notOver, ""},
		"other kinds so far":                   {[]HistoryMessage{other(), other(), other(), other()}, false, needMore, ""},
		"other kinds, and no more to read":     {[]HistoryMessage{other(), other()}, true, notOver, ""},
		"other kinds newer than the marker":    {[]HistoryMessage{other(), other(), other(), other(), idle("succeeded"), assistant("stop")}, false, over, ""},
		"a user message newer than the marker": {[]HistoryMessage{user(), idle("succeeded"), assistant("stop")}, false, notOver, ""},
		"interrupted":                          {[]HistoryMessage{idle("interrupted"), assistant("error")}, false, over, `outcome "interrupted"`},
		"failed":                               {[]HistoryMessage{idle("failed"), assistant("error")}, false, over, `outcome "failed"`},
		"a last step that did not stop":        {[]HistoryMessage{idle("succeeded"), assistant("tool-calls")}, false, over, `finished with "tool-calls"`},
		"a last step that is not finished":     {[]HistoryMessage{idle("succeeded"), assistant("")}, false, over, `finished with ""`},
		"the assistant message not read yet":   {[]HistoryMessage{idle("succeeded"), other(), other(), other()}, false, needMore, ""},
		"the assistant message not there":      {[]HistoryMessage{idle("succeeded"), other(), other(), other()}, true, over, "no assistant message is among"},
		"a prompt with no answer":              {[]HistoryMessage{idle("succeeded"), user()}, false, over, "no assistant message answers"},
		"another turn's marker next":           {[]HistoryMessage{idle("succeeded"), idle("succeeded"), assistant("stop")}, false, over, "no assistant message lies between"},
	} {
		got, err := judgeTurn(tc.msgs, tc.final)
		if got != tc.want || (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%s: judgeTurn = %v, %v; want %v, error containing %q", name, got, err, tc.want, tc.err)
		}
	}
}

func TestCertifyTurn(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		history func(f *fakeOpenCode)
		err     string
	}{
		"the live shape": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("tool-calls")
			f.addAssistant("stop")
			f.addIdle("succeeded")
		}, ""},
		"interrupted": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("error")
			f.addIdle("interrupted")
		}, `outcome "interrupted"`},
		"failed": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("error")
			f.addIdle("failed")
		}, `outcome "failed"`},
		"a last step that went on": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("tool-calls")
			f.addIdle("succeeded")
		}, "tool-calls"},
		"no marker": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("stop")
		}, "does not show the turn over"},
		// The turn went through, and entries of other kinds lie between the newest of
		// the history and the marker, more than a page of them.
		"the marker behind a page of other kinds": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("tool-calls")
			f.addAssistant("stop")
			f.addIdle("succeeded")
			for i := 0; i < 6; i++ {
				f.addOther("skill")
			}
		}, ""},
		"an interrupted marker behind a page of other kinds": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("error")
			f.addIdle("interrupted")
			for i := 0; i < 6; i++ {
				f.addOther("skill")
			}
		}, `outcome "interrupted"`},
		"the assistant message behind a page of other kinds": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("stop")
			for i := 0; i < 6; i++ {
				f.addOther("skill")
			}
			f.addIdle("succeeded")
		}, ""},
		"a prompt newer than the marker": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("stop")
			f.addIdle("succeeded")
			f.addUser("another prompt")
		}, "does not show the turn over"},
		"a marker further back than the bound": {func(f *fakeOpenCode) {
			f.addUser("the primer")
			f.addAssistant("stop")
			f.addIdle("succeeded")
			for i := 0; i < recentWindow*maxHistoryPages; i++ {
				f.addOther("skill")
			}
		}, "does not show the turn over"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			tc.history(f)
			err := CertifyTurn(ctx, f.client(), testSession, 50*time.Millisecond, 5*time.Millisecond)
			if (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("CertifyTurn = %v, want error containing %q", err, tc.err)
			}
		})
	}
}

// TestCertifyTurnWaitsForTheMarker: the marker is written when the turn ends, which
// may be a moment after the run that waited for it has printed its last event.
func TestCertifyTurnWaitsForTheMarker(t *testing.T) {
	f := newFake(t)
	f.addUser("the primer")
	f.addAssistant("stop")
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.addIdle("succeeded")
	}()
	if err := CertifyTurn(context.Background(), f.client(), testSession, 5*time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("CertifyTurn = %v", err)
	}
}

func TestCertifyTurnFailsForAServerThatIsNotThereOrDoesNotKnowTheSession(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.exists = false })
	if err := CertifyTurn(context.Background(), f.client(), testSession, time.Second, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("CertifyTurn of an unknown session = %v", err)
	}
	f.set(func() { f.exists = true })
	c := f.client()
	f.stop()
	if err := CertifyTurn(context.Background(), c, testSession, time.Second, 5*time.Millisecond); err == nil || !IsTransport(err) {
		t.Fatalf("CertifyTurn of a server that is down = %v, want a transport error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CertifyTurn(ctx, c, testSession, time.Second, 5*time.Millisecond); err == nil {
		t.Fatal("CertifyTurn on a canceled context returned no error")
	}
}
