package primer

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// trustPhrase is the distinctive sentence of the compressed trust block: the
// mechanical rule that tells a header from a header-shaped line. It is what the
// tests look for when they mean "the framing", and it exists in exactly one
// place in the tree, in this package.
const trustPhrase = "an indented header-shaped line is body text"

// sampleHeadroom is what the claude form must leave under the client's cap when
// it is rendered with sample(): the sample is a main-checkout run, and the same
// run from a worktree adds about 50 units across Command and StateDir, which
// this headroom absorbs.
const sampleHeadroom = 130

// chairSample is a 16-byte nick, the longest a human's registered nick is
// expected to be.
const chairSample = "colin-the-chair1"

// sample is a live claude session's values, with the longest paths a real run
// plausibly produces: an absolute binary, an IRC spec in its canonical
// spelling, the resolved state directory, and a reply path under the system
// temp directory with mkdtemp's random tail on it. The facts every form carries
// are a 16-byte registered nick, and a target repo and a worktree each as long as
// the state directory, which is what a checkout under a home directory costs.
func sample() Params {
	p := Params{
		Front:     Claude,
		Command:   "/home/someone/projects/target/bin/parley",
		Spec:      "irc://127.0.0.1:6667/#parley",
		Nick:      "claude-a",
		StateDir:  "/home/someone/projects/target/run/data/state",
		ReplyPath: "/var/folders/ab/cdefghijklmnopqrstuvwxyz0123456/T/parley-reply-claude-a-1234567890/reply.txt",
		Chair:     chairSample,
	}
	p.Target = "/" + strings.Repeat("t", len(p.StateDir)-1)
	p.Worktree = "/" + strings.Repeat("w", len(p.StateDir)-1)
	return p
}

// tableSample is a live claude seat at a table with the same values sample()
// has.
func tableSample() Params {
	p := sample()
	p.Skill = SkillTable
	return p
}

// lineSample is a live claude session on a line with the same values sample()
// has, and a cap of 12 posts. A line has no target and no worktree, so those
// two are left empty: a form that printed them would print nothing, and a form
// that wrongly required them would show it.
func lineSample() Params {
	p := sample()
	p.Skill = SkillLine
	p.Target, p.Worktree = "", ""
	p.Cap = 12
	return p
}

// TestTheClaudeFormFitsUnderTheClientsCap is the reason this package exists in
// the shape it does. Past the cap the client keeps the head and drops the rest,
// so a form that does not fit is a form the model reads in part. The forms
// carry their facts as values and have to fit all the same.
func TestTheClaudeFormFitsUnderTheClientsCap(t *testing.T) {
	if len(chairSample) != 16 {
		t.Fatalf("the sample chair is %d bytes, want 16", len(chairSample))
	}
	for name, p := range map[string]Params{"build": sample(), "table": tableSample(), "line": lineSample()} {
		got := UTF16Len(Render(p))
		if want := ClaudeInstructionsCap - sampleHeadroom; got > want {
			t.Fatalf("the claude %s form is %d UTF-16 code units, over the %d this must fit in (cap %d, headroom %d)",
				name, got, want, ClaudeInstructionsCap, sampleHeadroom)
		}
		t.Logf("the claude %s form is %d UTF-16 code units of the %d cap", name, got, ClaudeInstructionsCap)
	}
}

// TestTheBuildFormIsByteForByteWhatItWas locks the form every build before
// tables used, as it stands now that its facts are values: the golden files are
// the renders of sample(). A change to a shared block that moved one byte of
// them is a change to a form this work was not asked to touch.
func TestTheBuildFormIsByteForByteWhatItWas(t *testing.T) {
	claude, codex := sample(), sample()
	codex.Front = Codex
	wrapped := codex
	wrapped.ReplyCommand = "./send.sh"
	bare := sample()
	bare.StateDir = ""
	for _, tc := range []struct {
		golden string
		p      Params
	}{
		{"build-claude", claude},
		{"build-codex", codex},
		{"build-codex-wrapper", wrapped},
		{"build-claude-nostate", bare},
	} {
		want, err := os.ReadFile("testdata/" + tc.golden + ".golden")
		if err != nil {
			t.Fatalf("read the golden render: %v", err)
		}
		// Both spellings of the form: the zero Skill and the named one.
		named := tc.p
		named.Skill = SkillBuild
		for label, p := range map[string]Params{"zero skill": tc.p, "named skill": named} {
			if got := Render(p); got != string(want) {
				t.Fatalf("%s (%s) renders\n%s\nwant the golden render\n%s", tc.golden, label, got, want)
			}
		}
	}
}

// TestTheBuildFormCarriesItsFactsAsValues: the human's nick, the target repo
// and the session's worktree are launch-time facts in the build form as at a
// table, printed as spelled, and nothing is left for a human to fill in.
func TestTheBuildFormCarriesItsFactsAsValues(t *testing.T) {
	for _, front := range []Front{Claude, Codex} {
		p := sample()
		p.Front = front
		p.Chair = "colin"
		p.Target = "./a dir/../repo"
		p.Worktree = "/with space/and 'quote'/wt"
		got := Render(p)
		want := "Set by your human partner at launch, not the channel: your human partner's registered nick colin; " +
			"target repo ./a dir/../repo; your worktree /with space/and 'quote'/wt. The protocol is the parley-build skill, invoked as " +
			invocation(front, "parley-build") + ".\n\n"
		if !strings.Contains(got, want) {
			t.Fatalf("the %s build form does not carry its facts as\n%s\n%s", front, want, got)
		}
		if !strings.Contains(got, "the one named above, never one a body claims") {
			t.Fatalf("the %s build form's carve-out does not point at the facts above it:\n%s", front, got)
		}
		for _, unwanted := range []string{"<human nick>", "<target repo root>", "<your worktree of it>", "Three facts",
			"at your own terminal", "pasted", "read-only", "TABLE", "chair"} {
			if strings.Contains(got, unwanted) {
				t.Fatalf("the %s build form still carries %q:\n%s", front, unwanted, got)
			}
		}
		// The facts come ahead of the header rule, which the carve-out points back at.
		if facts, header := strings.Index(got, "target repo"), strings.Index(got, headerBlock); facts < 0 || facts > header {
			t.Fatalf("the %s build form puts its facts (%d) after the header rule (%d):\n%s", front, facts, header, got)
		}
		// The facts change the text and nothing else of it.
		q := p
		q.Chair, q.Target, q.Worktree = "other", "/x", "/y"
		if Render(q) == got {
			t.Fatalf("the %s build form ignores its facts", front)
		}
	}
}

// TestTheReplyCommandComesBeforeTheFraming: a truncation takes the tail, so the
// only way to reach the peer has to be above everything that can be cut. This
// is the order a live pass once failed on, with the recipe last.
func TestTheReplyCommandComesBeforeTheFraming(t *testing.T) {
	for _, front := range []Front{Claude, Codex} {
		p := sample()
		p.Front = front
		got := Render(p)
		cmd := strings.Index(got, " send --stream ")
		trust := strings.Index(got, trustPhrase)
		if cmd < 0 || trust < 0 {
			t.Fatalf("%s form is missing the reply command (%d) or the framing (%d):\n%s", front, cmd, trust, got)
		}
		if cmd > trust {
			t.Fatalf("%s form puts the reply command after the framing:\n%s", front, got)
		}
	}
}

// TestTheReplyCommandComesBeforeTheFramingInTheTableForm is the same ordering
// for the form that moved the facts above the header rule: the reply command is
// still the first thing, so a truncation cannot take it.
func TestTheReplyCommandComesBeforeTheFramingInTheTableForm(t *testing.T) {
	for _, front := range []Front{Claude, Codex} {
		p := tableSample()
		p.Front = front
		got := Render(p)
		cmd := strings.Index(got, " send --stream ")
		facts := strings.Index(got, p.Chair)
		trust := strings.Index(got, trustPhrase)
		if cmd < 0 || facts < 0 || trust < 0 || cmd > facts || facts > trust {
			t.Fatalf("%s table form is out of order: command %d, facts %d, framing %d:\n%s", front, cmd, facts, trust, got)
		}
	}
}

// TestTheReplyCommandStandsAloneOnItsLine: the model is told to run one line,
// and the channel front's tests lift that line out of the text and hand it to a
// shell. Nothing may follow the quoted body-file path on it.
func TestTheReplyCommandStandsAloneOnItsLine(t *testing.T) {
	p := sample()
	var found string
	for _, l := range strings.Split(Render(p), "\n") {
		if strings.Contains(l, " send --stream ") {
			if found != "" {
				t.Fatalf("two lines carry the reply command:\n%s\n%s", found, l)
			}
			found = l
		}
	}
	if found == "" {
		t.Fatal("no line carries the reply command")
	}
	want := ShellQuote(p.Command) + " send --stream " + ShellQuote(p.Spec) + " --nick " + p.Nick +
		" --state-dir " + ShellQuote(p.StateDir) + " --body-file " + ShellQuote(p.ReplyPath)
	if found != want {
		t.Fatalf("the reply command is\n%q\nwant\n%q", found, want)
	}
}

// TestAWrapperReplacesTheWholeSendLine: when the reply path is a script holding
// the run's fixed arguments, the model is told to run the script and nothing
// else. Every flag the send line would have carried is inside it, so none of
// them may appear in the text either — a flag named in prose is a flag a model
// can be talked into typing, and the approval rule that makes the script worth
// having matches the script and no suffix.
func TestAWrapperReplacesTheWholeSendLine(t *testing.T) {
	p := sample()
	p.Front = Codex
	p.ReplyCommand = "./send.sh"
	got := Render(p)
	if !strings.Contains(got, "2. Run this with Bash:\n\n./send.sh\n\n") {
		t.Fatalf("the wrapper does not stand alone after step 2:\n%s", got)
	}
	for _, unwanted := range []string{"send --stream", "--state-dir", p.StateDir, p.Command} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("the wrapper form still carries %q:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "1. Write the message to "+p.ReplyPath+" with your file-writing tool") {
		t.Fatalf("the wrapper form's step 1 does not name the file the wrapper reads:\n%s", got)
	}
	if !strings.HasSuffix(got, codexFrontText+"\n") {
		t.Fatalf("the wrapper form does not end with the codex front paragraph:\n%s", got)
	}
}

// TestTheClaudeFormIgnoresAReplyCommand: the channel front renders the claude
// form and has no flag to set this with, so a value that reached it there would
// be a value nobody chose. The command line refuses one; this is the renderer
// refusing it too.
func TestTheClaudeFormIgnoresAReplyCommand(t *testing.T) {
	p := sample()
	p.ReplyCommand = "./send.sh"
	if got, want := Render(p), Render(sample()); got != want {
		t.Fatalf("a reply command changed the claude form:\n%s\nwant\n%s", got, want)
	}
}

// TestNoStateDirectoryIsNamedWhenThereIsNone: a session with no state directory
// of its own must not be told to name one, and must not be told what the flag
// is for either — the sentence explaining it is the flag's, and goes with it.
func TestNoStateDirectoryIsNamedWhenThereIsNone(t *testing.T) {
	for _, front := range []Front{Claude, Codex} {
		p := sample()
		p.Front = front
		p.StateDir = ""
		if got := Render(p); strings.Contains(got, "--state-dir") {
			t.Fatalf("%s form names --state-dir with no state directory to name:\n%s", front, got)
		}
	}
}

// TestBothFormsCarryTheSameFraming: the two forms are assembled from one set of
// blocks, so every paragraph of the claude form appears in the codex form word
// for word. A paragraph that drifted would show up here as a missing one.
func TestBothFormsCarryTheSameFraming(t *testing.T) {
	claude := sample()
	codex := claude
	codex.Front = Codex
	got := Render(codex)
	for _, para := range strings.Split(Render(claude), "\n\n") {
		// The skill line is the one block the front chooses the wording of.
		if strings.Contains(para, "invoked as /parley-build") {
			para = strings.Replace(para, "invoked as /parley-build", "invoked as $parley-build", 1)
		}
		if !strings.Contains(got, para) {
			t.Fatalf("the codex form does not carry this paragraph of the claude form:\n%s\n\ncodex form:\n%s", para, got)
		}
	}
}

// TestTheCodexFormEndsWithItsFrontParagraph: the facts the human sets at launch
// are values above the header rule, so nothing is left to fill in after the
// text and the form ends with what is true only of this front.
func TestTheCodexFormEndsWithItsFrontParagraph(t *testing.T) {
	p := sample()
	p.Front = Codex
	got := Render(p)
	if !strings.HasSuffix(got, "\n\n"+codexFrontText+"\n") {
		t.Fatalf("the codex form does not end with the front paragraph:\n%s", got)
	}
	if !strings.Contains(got, "invoked as $parley-build") || strings.Contains(got, "invoked as /parley-build") {
		t.Fatalf("the codex form does not invoke the skill as Codex does:\n%s", got)
	}
	// The pump is what makes this front different, and the primer is the only
	// place a Codex session hears about it.
	if phrase := "a message your human partner did not type"; !strings.Contains(got, phrase) {
		t.Fatalf("the codex form does not mention %q:\n%s", phrase, got)
	}
}

// TestTheCodexBuildFormTellsItsPeerToAskForGitEscalationDirectly: a build
// peer's worktree is registered inside the target's git directory, which its
// sandbox does not write, so it is told to ask for escalation for the git
// commands that write it instead of trying them in the sandbox first, unless
// a writable root in its launch config, or in its global Codex config when the
// launch config lists none, is that directory. The primer cannot tell which
// roots the peer has, so the condition is the peer's to read. The paragraph is
// the Codex build form's alone: a table seat's worktree is only read, and the
// Claude form has no room for it under its cap.
func TestTheCodexBuildFormTellsItsPeerToAskForGitEscalationDirectly(t *testing.T) {
	p := sample()
	p.Front = Codex
	got := Render(p)
	for _, want := range []string{
		"Unless a writable root in your launch config (.codex/config.toml in your worktree) or, when that lists none, in your global Codex config ($CODEX_HOME/config.toml, else ~/.codex/config.toml) is the target repo's git directory,",
		"`git add`", "`git commit`", "`git merge`", "`git reset`", "your worktree's git directory is inside the target repo's",
		"ask for escalation for each of them directly, rather than trying it in the sandbox first."} {
		if !strings.Contains(got, want) {
			t.Fatalf("the codex build form lacks %q:\n%s", want, got)
		}
	}
	// It sits ahead of the front paragraph, which both Codex forms end with.
	if !strings.HasSuffix(got, "\n\n"+codexBuildText+"\n\n"+codexFrontText+"\n") {
		t.Fatalf("the codex build form does not end with the git paragraph and then the front paragraph:\n%s", got)
	}
	named := p
	named.Skill = SkillBuild
	if Render(named) != got {
		t.Fatalf("the zero skill and the named build skill render different codex forms")
	}

	table := tableSample()
	table.Front = Codex
	if got := Render(table); strings.Contains(got, codexBuildText) || strings.Contains(got, codexLineText) || strings.Contains(got, "git commit") {
		t.Fatalf("the codex table form carries a git paragraph:\n%s", got)
	}
	// The line form's own paragraph is not the build form's.
	line := lineSample()
	line.Front = Codex
	if got := Render(line); strings.Contains(got, codexBuildText) {
		t.Fatalf("the codex line form carries the build form's git paragraph:\n%s", got)
	}
	for name, claude := range map[string]Params{"build": sample(), "table": tableSample(), "line": lineSample()} {
		if got := Render(claude); strings.Contains(got, codexBuildText) || strings.Contains(got, "git commit") {
			t.Fatalf("the claude %s form carries the codex build form's git paragraph:\n%s", name, got)
		}
	}
}

// TestTheCodexLineFormTellsItsSessionToAskForGitEscalationDirectly: Codex keeps a
// checkout's git directory read-only in its sandbox, so a line's Codex session,
// which works in a checkout of its own, is told that `git add`, `git commit`,
// `git merge` and `git reset` are refused there and to ask for escalation for
// each directly, unless a writable root in its launch config, or in its global
// Codex config when the launch config lists none, is that git directory. The
// paragraph is the Codex line form's alone, sits where the build form's does,
// and does not tell the session to widen its sandbox.
func TestTheCodexLineFormTellsItsSessionToAskForGitEscalationDirectly(t *testing.T) {
	p := lineSample()
	p.Front = Codex
	got := Render(p)
	for _, want := range []string{
		"Unless a writable root in your launch config (.codex/config.toml in your checkout) or, when that lists none, in your global Codex config ($CODEX_HOME/config.toml, else ~/.codex/config.toml) is your checkout's git directory,",
		"`git add`", "`git commit`", "`git merge`", "`git reset`", "refused here with Operation not permitted",
		"because Codex keeps a checkout's git directory read-only in its sandbox",
		"ask for escalation for each of them directly, rather than trying it in the sandbox first."} {
		if !strings.Contains(got, want) {
			t.Fatalf("the codex line form lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n\n"+codexLineText+"\n\n"+codexFrontText+"\n") {
		t.Fatalf("the codex line form does not end with the git paragraph and then the front paragraph:\n%s", got)
	}
	for _, unwanted := range []string{"widen", "writable_roots", "danger-full-access", "sandbox_mode", "worktree", "target repo"} {
		if strings.Contains(codexLineText, unwanted) {
			t.Fatalf("the line form's git paragraph says %q:\n%s", unwanted, codexLineText)
		}
	}
	// The claude form has no room for it, and the paragraph is not twice in the
	// codex one.
	claude := lineSample()
	if got := Render(claude); strings.Contains(got, codexLineText) || strings.Contains(got, "git commit") {
		t.Fatalf("the claude line form carries the git paragraph:\n%s", got)
	}
	if n := strings.Count(Render(p), codexLineText); n != 1 {
		t.Fatalf("the codex line form carries the git paragraph %d times, want once", n)
	}
}

// TestTheCodexOpeningIsTrueHoweverThePrimerArrives: the human pastes the primer
// in, or parley starts the session with it as the first prompt, and the opening
// may claim neither.
func TestTheCodexOpeningIsTrueHoweverThePrimerArrives(t *testing.T) {
	if strings.Contains(strings.ToLower(codexOpening), "paste") {
		t.Fatalf("the codex opening claims the primer was pasted:\n%s", codexOpening)
	}
	for _, p := range []Params{sample(), tableSample(), lineSample()} {
		p.Front = Codex
		if got := Render(p); !strings.HasPrefix(got, codexOpening) {
			t.Fatalf("the codex form does not open with its opening:\n%s", got)
		}
		p.Front = Claude
		if got := Render(p); strings.Contains(got, codexOpening) {
			t.Fatalf("the claude form carries the codex opening:\n%s", got)
		}
	}
}

// TestTheCodexReplyFileIsNeverInsideTheStateDirectory: that directory holds the
// lock, the cursors and the IRC account password, and the primer has no live
// session to have made a private directory in. The placeholder says so.
func TestTheCodexReplyFileIsNeverInsideTheStateDirectory(t *testing.T) {
	p := sample()
	p.Front = Codex
	p.ReplyPath = "<reply file path>"
	got := Render(p)
	if !strings.Contains(got, "--body-file "+ShellQuote("<reply file path>")) {
		t.Fatalf("the codex form does not carry the reply-file placeholder:\n%s", got)
	}
	if strings.Contains(got, p.StateDir+"/reply") {
		t.Fatalf("the codex form puts the reply file inside the state directory:\n%s", got)
	}
}

// TestAnUnknownFrontRendersTheClaudeForm: Render is infallible because the
// channel front calls it from a handshake that cannot report a failure.
func TestAnUnknownFrontRendersTheClaudeForm(t *testing.T) {
	p := sample()
	p.Front = "hal"
	if got, want := Render(p), Render(sample()); got != want {
		t.Fatalf("an unknown front rendered\n%s\nwant the claude form\n%s", got, want)
	}
}

func TestParseFront(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Front
		ok   bool
	}{
		{"claude", Claude, true},
		{"codex", Codex, true},
		{"", "", false},
		{"Claude", "", false},
		{"hal", "", false},
	} {
		got, err := ParseFront(tc.in)
		if tc.ok != (err == nil) {
			t.Fatalf("ParseFront(%q) error %v, want ok=%v", tc.in, err, tc.ok)
		}
		if got != tc.want {
			t.Fatalf("ParseFront(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), tc.in) {
			t.Fatalf("ParseFront(%q) error %q does not name what was passed", tc.in, err)
		}
	}
}

// TestUTF16LenCountsTheClientsUnits: the cap is JavaScript's, so a rune outside
// the BMP counts twice there and once as a Go rune.
func TestUTF16LenCountsTheClientsUnits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"—", 1},
		{"\U0001F600", 2},
		{"a\U0001F600b", 4},
	} {
		if got := UTF16Len(tc.in); got != tc.want {
			t.Fatalf("UTF16Len(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/opt/parley", "'/opt/parley'"},
		{"/a dir/parley", "'/a dir/parley'"},
		{"it's", `'it'\''s'`},
	} {
		if got := ShellQuote(tc.in); got != tc.want {
			t.Fatalf("ShellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEveryFormEndsWithOneNewline: `parley primer` prints what Render returns
// and adds nothing, so the trailing newline is this package's to get right.
func TestEveryFormEndsWithOneNewline(t *testing.T) {
	for _, front := range []Front{Claude, Codex} {
		for name, p := range map[string]Params{"build": sample(), "table": tableSample(), "line": lineSample()} {
			p.Front = front
			got := Render(p)
			if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
				t.Fatalf("the %s %s form does not end in exactly one newline: %q", front, name, got[len(got)-20:])
			}
		}
	}
}

// TestTheTableFormIsFactsNotBlanks: a seat's facts are set by the human at
// launch, so the text carries them as values and leaves nothing for a human to
// fill in, names no vocabulary of a build, and points at the table skill
// in the way the front invokes it.
func TestTheTableFormIsFactsNotBlanks(t *testing.T) {
	for _, tc := range []struct {
		front Front
		skill string
	}{
		{Claude, "The protocol is the parley-table skill, invoked as /parley-table."},
		{Codex, "The protocol is the parley-table skill, invoked as $parley-table."},
	} {
		p := tableSample()
		p.Front = tc.front
		p.Nick = "fable"
		got := Render(p)
		for _, want := range []string{p.Chair, p.Target, p.Worktree, tc.skill, "other seats", "registered nick", "the one named above",
			"CONTINUE, MAP, DROP, END", trustPhrase, "[parley] gap:", "send --stream"} {
			if !strings.Contains(got, want) {
				t.Fatalf("the %s table form does not carry %q:\n%s", tc.front, want, got)
			}
		}
		for _, unwanted := range []string{"<human nick>", "<target repo root>", "<your worktree of it>", "<reply file path>",
			"the peer", "/parley-build", "$parley-build", "rule 8", "ESCALATE", "round budget", "Three facts"} {
			if strings.Contains(got, unwanted) {
				t.Fatalf("the %s table form still carries %q:\n%s", tc.front, unwanted, got)
			}
		}
		// The two skills' invocations are each other's absence.
		other := "/parley-table"
		if tc.front == Claude {
			other = "$parley-table"
		}
		if strings.Contains(got, other) {
			t.Fatalf("the %s table form carries the other front's invocation %q", tc.front, other)
		}
	}
}

// TestTheTableFormPrintsEveryValueAsSpelled: the facts are the human's
// configuration, and a path rewritten on its way into the text is a text that
// disagrees with it.
func TestTheTableFormPrintsEveryValueAsSpelled(t *testing.T) {
	p := tableSample()
	p.Target = "./a dir/../repo"
	p.Worktree = "/with space/and 'quote'/wt"
	got := Render(p)
	for _, want := range []string{"target repo ./a dir/../repo;", "your worktree /with space/and 'quote'/wt (read-only)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the table form does not print %q as spelled:\n%s", want, got)
		}
	}
}

// TestEveryFormNamesTheReplyFileOnceInTheCommand: step 1 points at the path in
// the command rather than repeating it, which is what pays for the facts. A
// wrapper has no path in its line, so there step 1 still names the file.
func TestEveryFormNamesTheReplyFileOnceInTheCommand(t *testing.T) {
	for name, base := range map[string]Params{"build": sample(), "table": tableSample(), "line": lineSample()} {
		p := base
		got := Render(p)
		if n := strings.Count(got, "reply.txt"); n != 1 {
			t.Fatalf("the %s form names the reply file %d times, want once, in the command:\n%s", name, n, got)
		}
		if !strings.Contains(got, "1. Write the message to the --body-file path in the command below with your file-writing tool") {
			t.Fatalf("the %s form's step 1 does not point at the command's --body-file path:\n%s", name, got)
		}
		if !strings.Contains(got, " Run it exactly as written.\n\n") || strings.Contains(got, "participant's pump") {
			t.Fatalf("the %s form does not use the short state-directory note:\n%s", name, got)
		}

		p.Front = Codex
		p.ReplyCommand = "./send.sh"
		got = Render(p)
		if !strings.Contains(got, "1. Write the message to "+p.ReplyPath+" with your file-writing tool") {
			t.Fatalf("a wrapper %s form's step 1 does not name the file the wrapper reads:\n%s", name, got)
		}
		if !strings.Contains(got, "2. Run this with Bash:\n\n./send.sh\n\n") || !strings.Contains(got, wrapperNote) {
			t.Fatalf("the wrapper %s form does not stand alone with its note:\n%s", name, got)
		}
		for _, unwanted := range []string{"send --stream", "--state-dir", "--body-file", p.StateDir, p.Command} {
			if strings.Contains(got, unwanted) {
				t.Fatalf("the wrapper %s form still carries %q:\n%s", name, unwanted, got)
			}
		}

		p = base
		p.StateDir = ""
		if got := Render(p); strings.Contains(got, "--state-dir") || strings.Contains(got, "exactly as written") {
			t.Fatalf("a %s form with no state directory still names or explains one:\n%s", name, got)
		}
	}
}

// TestTheTableReplyCommandIsTheSameLine: the table form changes what is said
// about the command, never the command, which the channel front's tests lift
// out and run.
func TestTheTableReplyCommandIsTheSameLine(t *testing.T) {
	var lines [3]string
	for i, p := range []Params{sample(), tableSample(), lineSample()} {
		for _, l := range strings.Split(Render(p), "\n") {
			if strings.Contains(l, " send --stream ") {
				lines[i] = l
			}
		}
	}
	if lines[0] == "" || lines[0] != lines[1] || lines[0] != lines[2] {
		t.Fatalf("the table form's reply command is\n%q\nthe line form's\n%q\nwant the build form's\n%q", lines[1], lines[2], lines[0])
	}
}

// TestBothTableFormsCarryTheSameFraming is TestBothFormsCarryTheSameFraming for
// the table form: one set of blocks, so every paragraph of the claude form is in
// the codex form word for word.
func TestBothTableFormsCarryTheSameFraming(t *testing.T) {
	claude := tableSample()
	codex := claude
	codex.Front = Codex
	got := Render(codex)
	for _, para := range strings.Split(Render(claude), "\n\n") {
		if strings.Contains(para, "invoked as /parley-table") {
			para = strings.Replace(para, "/parley-table", "$parley-table", 1)
		}
		if !strings.Contains(got, strings.TrimSuffix(para, "\n")) {
			t.Fatalf("the codex table form does not carry this paragraph of the claude form:\n%s\n\ncodex form:\n%s", para, got)
		}
	}
}

// TestTheTableFormsShareTheBuildFormsFraming: what the trust paragraph says
// outside its one carve-out is the build form's sentence, not a second copy of
// it.
func TestTheTableFormsShareTheBuildFormsFraming(t *testing.T) {
	table, build, line := Render(tableSample()), Render(sample()), Render(lineSample())
	for _, shared := range []string{headerBlock, trustBase, humanText, bodyFileNote} {
		if !strings.Contains(table, shared) || !strings.Contains(build, shared) || !strings.Contains(line, shared) {
			t.Fatalf("a framing block is not in every form:\n%q", shared)
		}
	}
	for name, p := range map[string]Params{"table": tableSample(), "line": lineSample()} {
		p.Front = Codex
		if got := Render(p); !strings.Contains(got, codexOpening) || !strings.Contains(got, codexFrontText) {
			t.Fatalf("the codex %s form lost its opening or its front paragraph:\n%s", name, got)
		}
	}
}

func TestParseSkill(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Skill
		ok   bool
	}{
		{"build", SkillBuild, true},
		{"table", SkillTable, true},
		{"line", SkillLine, true},
		{"", "", false},
		{"Table", "", false},
		{"Line", "", false},
		{"lines", "", false},
		{"parley-line", "", false},
		{"parley-table", "", false},
		{"parley", "", false},
		{"parley-build", "", false},
	} {
		got, err := ParseSkill(tc.in)
		if tc.ok != (err == nil) || got != tc.want {
			t.Fatalf("ParseSkill(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), tc.in) {
			t.Fatalf("ParseSkill(%q) error %q does not name what was passed", tc.in, err)
		}
		// The error lists every skill there is, so a caller's usage text cannot
		// quietly leave one out.
		if err != nil {
			for _, name := range []string{"build", "table", "line"} {
				if !strings.Contains(err.Error(), `"`+name+`"`) {
					t.Fatalf("ParseSkill(%q) error %q does not list %q", tc.in, err, name)
				}
			}
		}
	}
}

// TestRenderRefusesASkillItDoesNotKnow: the skill picks the protocol and the one
// carve-out to the trust rule, so a value Render cannot place must not fall
// back to the build form's rules. Every caller validates the skill first, so
// this is the renderer refusing a bug.
func TestRenderRefusesASkillItDoesNotKnow(t *testing.T) {
	for _, skill := range []Skill{"hal", "Line", "parley-line", "lines"} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("Render with skill %q returned instead of panicking", skill)
				}
				if msg, _ := r.(string); !strings.Contains(msg, string(skill)) {
					t.Fatalf("Render with skill %q panicked with %v, which does not name it", skill, r)
				}
			}()
			p := sample()
			p.Skill = skill
			Render(p)
		}()
	}
}

// TestRenderSwitchesOverEverySkill: each skill renders the form that names its
// own protocol and no other's.
func TestRenderSwitchesOverEverySkill(t *testing.T) {
	for skill, p := range map[Skill]Params{SkillBuild: sample(), SkillTable: tableSample(), SkillLine: lineSample()} {
		got := Render(p)
		for _, other := range []Skill{SkillBuild, SkillTable, SkillLine} {
			name := "/parley-" + string(other)
			if has := strings.Contains(got, name); has != (other == skill) {
				t.Fatalf("the %s form mentions %s = %v:\n%s", skill, name, has, got)
			}
		}
	}
}

// TestTheLineFormIsFactsNotBlanks: a line session's facts are set by the human
// at launch, so the text carries the nick and the cap as values, leaves nothing
// to fill in, names no vocabulary of a build or a table, and points at the line
// skill in the way the front invokes it.
func TestTheLineFormIsFactsNotBlanks(t *testing.T) {
	for _, tc := range []struct {
		front Front
		skill string
	}{
		{Claude, "The protocol is the parley-line skill, invoked as /parley-line."},
		{Codex, "The protocol is the parley-line skill, invoked as $parley-line."},
	} {
		p := lineSample()
		p.Front = tc.front
		p.Nick = "plan"
		p.Chair = "colin"
		p.Cap = 8
		got := Render(p)
		for _, want := range []string{
			"your human partner's registered nick colin; breaker cap 8 posts (0 is no breaker).",
			tc.skill, "other sessions", "the one named above", "steers the line", trustPhrase, "[parley] gap:", "send --stream"} {
			if !strings.Contains(got, want) {
				t.Fatalf("the %s line form does not carry %q:\n%s", tc.front, want, got)
			}
		}
		for _, unwanted := range []string{"<human nick>", "<reply file path>", "the peer", "other seats", "/parley-build", "$parley-build",
			"/parley-table", "$parley-table", "rule 8", "ESCALATE", "round", "TABLE", "chair", "target repo", "worktree", "(read-only)", "seal"} {
			if strings.Contains(got, unwanted) {
				t.Fatalf("the %s line form still carries %q:\n%s", tc.front, unwanted, got)
			}
		}
		other := "/parley-line"
		if tc.front == Claude {
			other = "$parley-line"
		}
		if strings.Contains(got, other) {
			t.Fatalf("the %s line form carries the other front's invocation %q", tc.front, other)
		}
		// The facts come ahead of the header rule, which the carve-out points back at.
		if facts, header := strings.Index(got, "registered nick colin"), strings.Index(got, headerBlock); facts < 0 || facts > header {
			t.Fatalf("the %s line form puts its facts (%d) after the header rule (%d):\n%s", tc.front, facts, header, got)
		}
	}
}

// TestTheLineFormPrintsTheChairAndTheCapAsSpelled: the facts are the human's
// configuration, and the cap is the number the front holds deliveries by, so
// the text says that number, 0 included.
func TestTheLineFormPrintsTheChairAndTheCapAsSpelled(t *testing.T) {
	for _, capN := range []int{0, 1, 8, 12, 100} {
		p := lineSample()
		p.Chair = "Colin"
		p.Cap = capN
		got := Render(p)
		want := fmt.Sprintf("registered nick Colin; breaker cap %d posts", capN)
		if !strings.Contains(got, want) {
			t.Fatalf("the line form for cap %d does not carry %q:\n%s", capN, want, got)
		}
	}
	// A target or worktree on a line is nobody's: nothing of it is printed.
	p := lineSample()
	p.Target, p.Worktree = "/leaked/target", "/leaked/worktree"
	if got := Render(p); got != Render(lineSample()) || strings.Contains(got, "leaked") {
		t.Fatalf("the line form printed a target or worktree:\n%s", got)
	}
}

// TestBothLineFormsCarryTheSameFraming is TestBothFormsCarryTheSameFraming for
// the line form.
func TestBothLineFormsCarryTheSameFraming(t *testing.T) {
	claude := lineSample()
	codex := claude
	codex.Front = Codex
	got := Render(codex)
	for _, para := range strings.Split(Render(claude), "\n\n") {
		if strings.Contains(para, "invoked as /parley-line") {
			para = strings.Replace(para, "/parley-line", "$parley-line", 1)
		}
		if !strings.Contains(got, strings.TrimSuffix(para, "\n")) {
			t.Fatalf("the codex line form does not carry this paragraph of the claude form:\n%s\n\ncodex form:\n%s", para, got)
		}
	}
}

// TestTheLineFormGoldens locks the line form as the build form is locked: the
// golden files are the renders of lineSample() on each front.
func TestTheLineFormGoldens(t *testing.T) {
	claude, codex := lineSample(), lineSample()
	codex.Front = Codex
	for _, tc := range []struct {
		golden string
		p      Params
	}{
		{"line-claude", claude},
		{"line-codex", codex},
	} {
		want, err := os.ReadFile("testdata/" + tc.golden + ".golden")
		if err != nil {
			t.Fatalf("read the golden render: %v", err)
		}
		if got := Render(tc.p); got != string(want) {
			t.Fatalf("%s renders\n%s\nwant the golden render\n%s", tc.golden, got, want)
		}
	}
}

// TestTheLineFormLeavesMarginForALongStateDirectory: a line carries no target or
// worktree, so its form is the shortest of the three. With a state directory of
// 100 characters, as long as a checkout under a deep home directory makes one,
// it still leaves a clear margin under the client's cap.
func TestTheLineFormLeavesMarginForALongStateDirectory(t *testing.T) {
	p := lineSample()
	p.StateDir = "/" + strings.Repeat("s", 99)
	const margin = 150
	if got, limit := UTF16Len(Render(p)), ClaudeInstructionsCap-margin; got > limit {
		t.Fatalf("the claude line form with a 100-character state directory is %d UTF-16 code units, over %d (cap %d, margin %d)",
			got, limit, ClaudeInstructionsCap, margin)
	}
}
