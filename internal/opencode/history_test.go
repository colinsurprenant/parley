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

func TestHumanStopped(t *testing.T) {
	for name, tc := range map[string]struct {
		recent []HistoryMessage // newest first
		want   bool
	}{
		"nothing yet":                                {nil, false},
		"interrupted, nothing after":                 {[]HistoryMessage{idle("interrupted"), assistant("error"), user()}, true},
		"interrupted, a user message after":          {[]HistoryMessage{user(), idle("interrupted"), assistant("error")}, false},
		"interrupted, then a turn that went through": {[]HistoryMessage{idle("succeeded"), assistant("stop"), user(), idle("interrupted")}, false},
		"succeeded":                                  {[]HistoryMessage{idle("succeeded"), assistant("stop"), user()}, false},
		"failed":                                     {[]HistoryMessage{idle("failed"), assistant("error")}, false},
		"no marker in the window":                    {[]HistoryMessage{assistant("tool-calls"), assistant("tool-calls")}, false},
		"other kinds are read past":                  {[]HistoryMessage{{ID: "msg_s", Kind: "skill"}, idle("interrupted")}, true},
	} {
		if got := humanStopped(tc.recent); got != tc.want {
			t.Errorf("%s: humanStopped = %v, want %v", name, got, tc.want)
		}
	}
}

func TestJudgeTurn(t *testing.T) {
	for name, tc := range map[string]struct {
		recent  []HistoryMessage
		settled bool
		err     string
	}{
		"succeeded after a step that stopped": {[]HistoryMessage{idle("succeeded"), assistant("stop"), assistant("tool-calls")}, true, ""},
		"empty":                               {nil, false, ""},
		"still running":                       {[]HistoryMessage{assistant("tool-calls"), user()}, false, ""},
		"interrupted":                         {[]HistoryMessage{idle("interrupted"), assistant("error")}, true, `outcome "interrupted"`},
		"failed":                              {[]HistoryMessage{idle("failed"), assistant("error")}, true, `outcome "failed"`},
		"a last step that did not stop":       {[]HistoryMessage{idle("succeeded"), assistant("tool-calls")}, true, `finished with "tool-calls"`},
		"a last step that is not finished":    {[]HistoryMessage{idle("succeeded"), assistant("")}, true, `finished with ""`},
		"no assistant message":                {[]HistoryMessage{idle("succeeded"), user()}, true, "no assistant message"},
		"another turn's marker next":          {[]HistoryMessage{idle("succeeded"), idle("succeeded"), assistant("stop")}, true, "no assistant message lies between"},
	} {
		settled, err := judgeTurn(tc.recent)
		if settled != tc.settled || (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%s: judgeTurn = %v, %v; want settled %v, error containing %q", name, settled, err, tc.settled, tc.err)
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
		}, "does not end in an idle marker"},
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
