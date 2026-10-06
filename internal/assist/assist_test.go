package assist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/alecf/heyman/internal/manpage"
)

// --- fakes ---

type fakeMan struct {
	mu      sync.Mutex
	pages   map[string]string // "name" or "name(section)"
	fetches []string
}

func (f *fakeMan) Fetch(name, section string) (string, error) {
	if err := manpage.ValidateName(name); err != nil {
		return "", err
	}
	key := pageKey(name, section)
	f.mu.Lock()
	f.fetches = append(f.fetches, key)
	f.mu.Unlock()
	if p, ok := f.pages[key]; ok {
		return p, nil
	}
	return "", fmt.Errorf("%w: %s", manpage.ErrNotFound, key)
}

func (f *fakeMan) Apropos(keyword string) (string, error) {
	return "ls(1) - list directory contents", nil
}

// step is one scripted model reply.
type step struct {
	text  string
	calls []fantasy.ToolCallContent
	err   error
}

func call(name string, in any) fantasy.ToolCallContent {
	raw, _ := json.Marshal(in)
	return fantasy.ToolCallContent{ToolCallID: fmt.Sprintf("%s-%d", name, len(raw)), ToolName: name, Input: string(raw)}
}

type fakeModel struct {
	mu    sync.Mutex
	steps []step
	calls []fantasy.Call
}

func (m *fakeModel) Generate(_ context.Context, c fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, c)
	if len(m.steps) == 0 {
		return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: ""}}, FinishReason: fantasy.FinishReasonStop}, nil
	}
	s := m.steps[0]
	m.steps = m.steps[1:]
	if s.err != nil {
		return nil, s.err
	}
	var content fantasy.ResponseContent
	if s.text != "" {
		content = append(content, fantasy.TextContent{Text: s.text})
	}
	finish := fantasy.FinishReasonStop
	for _, c := range s.calls {
		content = append(content, c)
		finish = fantasy.FinishReasonToolCalls
	}
	return &fantasy.Response{Content: content, FinishReason: finish, Usage: fantasy.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}

func (m *fakeModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *fakeModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *fakeModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *fakeModel) Provider() string { return "fake" }
func (m *fakeModel) Model() string    { return "fake" }

func toolNames(c fantasy.Call) []string {
	var names []string
	for _, t := range c.Tools {
		names = append(names, t.GetName())
	}
	return names
}

func systemText(c fantasy.Call) string {
	var b strings.Builder
	for _, m := range c.Prompt {
		if m.Role != fantasy.MessageRoleSystem {
			continue
		}
		for _, p := range m.Content {
			if t, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
				b.WriteString(t.Text)
			}
		}
	}
	return b.String()
}

func newAssistant(m *fakeModel, man *fakeMan) *Assistant {
	return &Assistant{Model: m, ModelName: "fake/fake", Man: man}
}

var lsPage = "LS(1)\n\nNAME\n     ls – list directory contents\n\n     -S      Sort by size\n     -l      Long format\n"

// --- Ask ---

func TestAskAnswerTool(t *testing.T) {
	man := &fakeMan{pages: map[string]string{"ls": lsPage, "sort": "SORT(1)\n -n numeric\n"}}
	m := &fakeModel{steps: []step{
		{text: "Let me check sort.", calls: []fantasy.ToolCallContent{
			call("man", manInput{Page: "sort", Search: "numeric"}),
			call("which", whichInput{Program: "sort"}),
			call("man_search", manSearchInput{Keyword: "list"}),
		}},
		{calls: []fantasy.ToolCallContent{call("answer", answerInput{Command: "`ls -lS`", Explanation: "sorts by size"})}},
	}}
	var events []Event
	a := newAssistant(m, man)
	a.OnEvent = func(e Event) { events = append(events, e) }
	res, err := a.Ask(context.Background(), Request{Command: "ls", Question: "sort by size"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command != "ls -lS" {
		t.Errorf("command = %q", res.Command)
	}
	if res.Steps != 2 || res.Usage.InputTokens != 200 {
		t.Errorf("steps=%d usage=%+v", res.Steps, res.Usage)
	}
	if strings.Join(res.ManPages, ",") != "ls,sort" {
		t.Errorf("man pages = %v", res.ManPages)
	}
	if len(res.ToolCalls) != 4 || len(events) != 4 {
		t.Errorf("tool calls = %+v, events = %d", res.ToolCalls, len(events))
	}
	// The named page was preloaded into the system prompt.
	if !strings.Contains(systemText(m.calls[0]), "Sort by size") {
		t.Error("ls page not preloaded")
	}
}

func TestAskTextReplyFallback(t *testing.T) {
	tests := []struct {
		name, text, want string
		err              error
	}{
		{"fenced", "Here you go:\n```bash\nls -lS\n```\nSorts by size.", "ls -lS", nil},
		{"plain", "ls -lS", "ls -lS", nil},
		{"refusal", "I cannot help with that.", "", ErrNoCommand},
		{"chatter", "Let me look at the man page first.", "", ErrNoCommand},
		{"empty", "", "", ErrNoCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &fakeModel{steps: []step{{text: tt.text}}}
			res, err := newAssistant(m, &fakeMan{pages: map[string]string{"ls": lsPage}}).Ask(context.Background(), Request{Command: "ls", Question: "q"})
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if res == nil {
				t.Fatal("nil result")
			}
			if res.Command != tt.want {
				t.Errorf("command = %q, want %q", res.Command, tt.want)
			}
		})
	}
}

func TestAskMaxStepsForcesAnswer(t *testing.T) {
	man := &fakeMan{pages: map[string]string{"ls": lsPage}}
	readLs := step{calls: []fantasy.ToolCallContent{call("man", manInput{Page: "ls"})}}
	m := &fakeModel{steps: []step{readLs, readLs,
		{calls: []fantasy.ToolCallContent{call("answer", answerInput{Command: "ls -S"})}},
	}}
	a := newAssistant(m, man)
	a.MaxSteps = 3
	res, err := a.Ask(context.Background(), Request{Question: "biggest files"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command != "ls -S" {
		t.Errorf("command = %q", res.Command)
	}
	if len(m.calls) != 3 {
		t.Fatalf("model calls = %d", len(m.calls))
	}
	if got := toolNames(m.calls[0]); len(got) != 4 {
		t.Errorf("first step tools = %v", got)
	}
	if got := toolNames(m.calls[2]); len(got) != 1 || got[0] != "answer" {
		t.Errorf("last step tools = %v, want only answer", got)
	}
	if !strings.Contains(systemText(m.calls[2]), "run out of tool calls") {
		t.Error("last step system prompt lacks the final-step note")
	}
}

func TestAskMaxStepsNoAnswer(t *testing.T) {
	readLs := step{calls: []fantasy.ToolCallContent{call("man", manInput{Page: "ls"})}}
	m := &fakeModel{steps: []step{readLs, readLs}}
	a := newAssistant(m, &fakeMan{pages: map[string]string{"ls": lsPage}})
	a.MaxSteps = 2
	res, err := a.Ask(context.Background(), Request{Question: "x"})
	if !errors.Is(err, ErrNoCommand) {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Steps != 2 || len(res.ToolCalls) == 0 {
		t.Errorf("partial result = %+v", res)
	}
}

func TestAskExplainRequired(t *testing.T) {
	m := &fakeModel{steps: []step{
		{calls: []fantasy.ToolCallContent{call("answer", answerInput{Command: "ls -S"})}},
		{calls: []fantasy.ToolCallContent{call("answer", answerInput{Command: "ls -S", Explanation: "by size"})}},
	}}
	res, err := newAssistant(m, &fakeMan{}).Ask(context.Background(), Request{Question: "x", Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Explanation != "by size" || len(m.calls) != 2 {
		t.Errorf("res = %+v, calls = %d", res, len(m.calls))
	}

	// A model that never adds the explanation still yields its command.
	m = &fakeModel{steps: []step{
		{calls: []fantasy.ToolCallContent{call("answer", answerInput{Command: "ls -S"})}},
		{text: ""},
	}}
	res, err = newAssistant(m, &fakeMan{}).Ask(context.Background(), Request{Question: "x", Explain: true})
	if err != nil || res.Command != "ls -S" {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestAskToolsUnsupportedFallback(t *testing.T) {
	m := &fakeModel{steps: []step{
		{err: errors.New(`registry.ollama.ai/library/gemma3:270m does not support tools`)},
		{text: "ls -S"},
	}}
	res, err := newAssistant(m, &fakeMan{pages: map[string]string{"ls": lsPage}}).Ask(context.Background(), Request{Command: "ls", Question: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command != "ls -S" || len(res.Notes) != 1 {
		t.Errorf("res = %+v", res)
	}
	if len(m.calls[1].Tools) != 0 {
		t.Error("fallback call still offered tools")
	}
	if !strings.Contains(systemText(m.calls[1]), "Sort by size") {
		t.Error("fallback lost the preloaded page")
	}
}

func TestAskOtherErrorsPropagate(t *testing.T) {
	m := &fakeModel{steps: []step{{err: errors.New("401 unauthorized")}}}
	if _, err := newAssistant(m, &fakeMan{}).Ask(context.Background(), Request{Question: "x"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestAskMissingManPage(t *testing.T) {
	m := &fakeModel{steps: []step{{text: "docker ps"}}}
	res, err := newAssistant(m, &fakeMan{}).Ask(context.Background(), Request{Command: "docker", Question: "list containers"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) != 1 || len(res.ManPages) != 0 {
		t.Errorf("res = %+v", res)
	}
	user := m.calls[0].Prompt[len(m.calls[0].Prompt)-1]
	if tp, _ := fantasy.AsMessagePart[fantasy.TextPart](user.Content[0]); tp.Text != "docker list containers" {
		t.Errorf("user prompt = %q", tp.Text)
	}
	if !strings.Contains(systemText(m.calls[0]), "has no man page") {
		t.Error("system prompt doesn't mention the missing page")
	}

	// With an explicit section, or an invalid name, it's an error.
	if _, err := newAssistant(&fakeModel{}, &fakeMan{}).Ask(context.Background(), Request{Command: "printf", Section: "3", Question: "x"}); err == nil {
		t.Error("missing section page: want error")
	}
	if _, err := newAssistant(&fakeModel{}, &fakeMan{}).Ask(context.Background(), Request{Command: "-rf", Question: "x"}); err == nil {
		t.Error("invalid name: want error")
	}
}

func TestAskEmptyQuestion(t *testing.T) {
	if _, err := newAssistant(&fakeModel{}, &fakeMan{}).Ask(context.Background(), Request{Question: "  "}); err == nil {
		t.Error("want error")
	}
}

// Parallel tool calls must not race (run with -race) and events must be
// serialized.
func TestAskParallelTools(t *testing.T) {
	pages := map[string]string{}
	var calls []fantasy.ToolCallContent
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("p%d", i)
		pages[name] = strings.Repeat("line "+name+"\n", 50)
		calls = append(calls, fantasy.ToolCallContent{ToolCallID: name, ToolName: "man", Input: fmt.Sprintf(`{"page":%q}`, name)})
		calls = append(calls, fantasy.ToolCallContent{ToolCallID: name + "s", ToolName: "man", Input: fmt.Sprintf(`{"page":%q,"search":"line"}`, name)})
	}
	m := &fakeModel{steps: []step{{calls: calls}, {calls: []fantasy.ToolCallContent{call("answer", answerInput{Command: "true"})}}}}
	inEvent := false
	var mu sync.Mutex
	a := newAssistant(m, &fakeMan{pages: pages})
	a.OnEvent = func(Event) {
		mu.Lock()
		if inEvent {
			t.Error("concurrent OnEvent")
		}
		inEvent = true
		mu.Unlock()
		mu.Lock()
		inEvent = false
		mu.Unlock()
	}
	res, err := a.Ask(context.Background(), Request{Question: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ManPages) != 20 || len(res.ToolCalls) != 41 {
		t.Errorf("pages=%d calls=%d", len(res.ManPages), len(res.ToolCalls))
	}
}

func TestReadManErrors(t *testing.T) {
	r := &run{a: &Assistant{Man: &fakeMan{pages: map[string]string{"ls": lsPage}}}, pages: map[string]string{}}
	r.a.defaults()
	for _, in := range []manInput{{Page: ""}, {Page: "--help"}, {Page: "../etc/passwd"}, {Page: "nope"}} {
		if _, err := r.readMan(in); err == nil {
			t.Errorf("readMan(%+v): want error", in)
		}
	}
	out, err := r.readMan(manInput{Page: "ls", Search: "size"})
	if err != nil || !strings.Contains(out, "Sort by size") {
		t.Errorf("search = %q, %v", out, err)
	}
}

// --- text helpers ---

func TestParseText(t *testing.T) {
	tests := []struct {
		in, cmd, expl string
	}{
		{"", "", ""},
		{"ls -la", "ls -la", ""},
		{"ls -la\n\nLists everything.", "ls -la", "Lists everything."},
		{"```bash\nfind . -name '*.go'\n```", "find . -name '*.go'", ""},
		{"Use this:\n```sh\n$ du -sh *\n```\nShows sizes.", "du -sh *", "Use this:\nShows sizes."},
		{"Here is the command:\nps aux", "ps aux", ""},
		{"`lsof -i :8080`", "lsof -i :8080", ""},
		{"I cannot help with that request.", "", ""},
		{"I'm sorry, but I can't do that.", "", ""},
		{"Sorry, I don't know.", "", ""},
		{"Unfortunately there is no such flag.", "", ""},
		{"The command is not available.", "", ""},
		{"Let me check the man page.", "", ""},
		{"As an AI model I can't run commands.", "", ""},
		{"```\n```", "", ""},
		{"echo `date`", "echo `date`", ""},
		{"let x=1+2", "let x=1+2", ""},
		{"test -f foo && echo yes", "test -f foo && echo yes", ""},
	}
	for _, tt := range tests {
		cmd, expl := ParseText(tt.in)
		if cmd != tt.cmd || expl != tt.expl {
			t.Errorf("ParseText(%q) = %q, %q; want %q, %q", tt.in, cmd, expl, tt.cmd, tt.expl)
		}
	}
}

func TestCleanCommand(t *testing.T) {
	tests := map[string]string{
		"  ls  ":                     "ls",
		"`ls -la`":                   "ls -la",
		"``ls``":                     "ls",
		"$ ls":                       "ls",
		"```bash\nls -la\n```":       "ls -la",
		"```\nls -la\n```":           "ls -la",
		"```ls -la```":               "ls -la",
		"echo `date`":                "echo `date`",
		"`echo `date``":              "echo `date`",
		"tar -czf a.tgz dir":         "tar -czf a.tgz dir",
		"```sh\nfind . | wc -l\n```": "find . | wc -l",
	}
	for in, want := range tests {
		if got := cleanCommand(in); got != want {
			t.Errorf("cleanCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChunk(t *testing.T) {
	page := strings.Repeat("0123456789\n", 10) // 110 chars
	if got := Chunk("x", page, 0, 1000); got != page {
		t.Error("short page should be returned whole without header")
	}
	got := Chunk("x", page, 0, 50)
	if !strings.HasPrefix(got, "[man x: characters 0-44 of 110; call again with offset=44") {
		t.Errorf("first chunk header: %q", got)
	}
	got = Chunk("x", page, 88, 50)
	if !strings.HasPrefix(got, "[man x: characters 88-110 of 110]") {
		t.Errorf("last chunk: %q", got)
	}
	if got := Chunk("x", page, 500, 50); !strings.Contains(got, "past the end") {
		t.Errorf("offset past end: %q", got)
	}
	if got := Chunk("x", page, -5, 1000); got != page {
		t.Error("negative offset should read from the start")
	}
	// Offsets and cuts never split UTF-8.
	uni := strings.Repeat("ls – list ‐ files ’ ok", 20)
	for off := 0; off < 40; off++ {
		for lim := 5; lim < 40; lim += 3 {
			body := Chunk("u", uni, off, lim)
			if !utf8.ValidString(body) {
				t.Fatalf("Chunk(off=%d, lim=%d) produced invalid UTF-8", off, lim)
			}
		}
	}
}

func TestCutAtLine(t *testing.T) {
	tests := []struct {
		s     string
		limit int
		want  string
	}{
		{"abc", 10, "abc"},
		{"aaaa\nbbbb\ncccc", 12, "aaaa\nbbbb\n"},
		{"a\nbbbbbbbbbbbbbbbbbb", 10, "a\nbbbbbbbb"}, // newline too early: hard cut
		{"ééééé", 3, "é"},
		{"abc", 0, ""},
	}
	for _, tt := range tests {
		if got := cutAtLine(tt.s, tt.limit); got != tt.want {
			t.Errorf("cutAtLine(%q, %d) = %q, want %q", tt.s, tt.limit, got, tt.want)
		}
	}
}

func TestGrep(t *testing.T) {
	page := "one\ntwo -n numeric\nthree\nfour\nfive\nsix -N other\nseven"
	got := Grep(page, "-n", 1, 1000)
	want := "[2 matching lines for \"-n\"]\none\ntwo -n numeric\nthree\n--\nfive\nsix -N other\nseven\n"
	if got != want {
		t.Errorf("Grep = %q, want %q", got, want)
	}
	if got := Grep(page, "zzz", 1, 1000); !strings.HasPrefix(got, "no lines match") {
		t.Errorf("no match: %q", got)
	}
	// Invalid regex falls back to a literal.
	if got := Grep("a [x\nb", "[x", 0, 1000); !strings.Contains(got, "a [x") {
		t.Errorf("literal fallback: %q", got)
	}
	if got := Grep(page, strings.Repeat("a", 500), 0, 1000); !strings.Contains(got, "too long") {
		t.Errorf("long pattern: %q", got)
	}
	// Output is capped even when everything matches.
	big := strings.Repeat("match this line\n", 10000)
	if got := Grep(big, "", 2, 2000); len(got) > 2000 || !strings.Contains(got, "truncated") {
		t.Errorf("cap: len=%d", len(got))
	}
	// Nested quantifiers are linear in Go's RE2; this must finish quickly.
	if got := Grep(strings.Repeat("a", 5000)+"!", "(a+)+$", 0, 1000); !strings.HasPrefix(got, "no lines") {
		t.Errorf("pathological: %q", got[:min(40, len(got))])
	}
}

func TestToolsUnsupported(t *testing.T) {
	for _, msg := range []string{
		"registry.ollama.ai/library/gemma3:270m does not support tools",
		"No endpoints found that support tool use",
		"tools param requires --jinja flag",
	} {
		if !toolsUnsupported(errors.New(msg)) {
			t.Errorf("%q not detected", msg)
		}
	}
	if toolsUnsupported(errors.New("connection refused")) {
		t.Error("false positive")
	}
}

func TestSystemPrompt(t *testing.T) {
	long := strings.Repeat("x\n", 100)
	sp := systemPrompt(Request{Command: "ls", Question: "q"}, long, 50, true, "")
	if !strings.Contains(sp, "[truncated: page continues") || !strings.Contains(sp, "`answer`") {
		t.Error("tools prompt missing truncation note or answer instruction")
	}
	sp = systemPrompt(Request{Question: "q"}, "", 50, false, "")
	if strings.Contains(sp, "man page below") || strings.Contains(sp, "<manpage") {
		t.Error("no-tools prompt without a command must not reference a man page below")
	}
	sp = systemPrompt(Request{Question: "q", Explain: true}, "", 50, false, "")
	if !strings.Contains(sp, "explanation") {
		t.Error("explain mode not mentioned")
	}
}

func TestPromptPreview(t *testing.T) {
	sys, user, err := PromptPreview(&fakeMan{}, Request{Command: "how", Question: "do I list files"})
	if err != nil {
		t.Fatal(err)
	}
	if user != "how do I list files" || !strings.Contains(sys, "`how`") {
		t.Errorf("preview: user=%q", user)
	}
}

func TestParseTextRejectsJunkFromEvals(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`man[ARGS]{"page": "stat"}`, ""},
		{`stat({"page": "stat"})`, ""},
		{`{"command": "ls"}`, ""},
		{"**find . -name '*.go'**", "find . -name '*.go'"},
		{"__ls -la__", "ls -la"},
	} {
		if got, _ := ParseText(tc.in); got != tc.want {
			t.Errorf("ParseText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAskDoesNotMutateAssistant(t *testing.T) {
	a := &Assistant{}
	_ = a.withDefaults()
	if a.MaxSteps != 0 || a.PageChars != 0 || a.Man != nil {
		t.Fatalf("withDefaults mutated the receiver: %+v", a)
	}
}

func TestAnswerFromMarkup(t *testing.T) {
	for _, tc := range []struct{ name, in, cmd, expl string }{
		{"qwen xml in reasoning, fenced, truncated tags",
			"`du -b` isn't portable.\n\n<tool_call>\n<function=answer>\n<parameter=command>\n```bash\nfind . -type f -exec stat -f '%z %N' {} \\;\n</parameter>\n</function>\n</tool_call>",
			`find . -type f -exec stat -f '%z %N' {} \;`, ""},
		{"qwen xml with explanation",
			"<function=answer><parameter=command>ls -lS</parameter><parameter=explanation>largest first</parameter></function>",
			"ls -lS", "largest first"},
		{"json tool call",
			`<tool_call>{"name": "answer", "arguments": {"command": "lsof -i :8080"}}</tool_call>`,
			"lsof -i :8080", ""},
		{"other tool ignored", `<tool_call>{"name": "man", "arguments": {"page": "ls"}}</tool_call>`, "", ""},
		{"plain reasoning with a draft command is not an answer", "maybe `ls -la` would work", "", ""},
		{"dangling closing tags after a fence",
			"Let me use the safer approach:\n```bash\nfind . -name '*.py' -exec cat {} \\; | wc -l\n</parameter>\n</function>\n</tool_call>",
			`find . -name '*.py' -exec cat {} \; | wc -l`, ""},
		{"final command sentence", "I found it in the man page.\n\nSo the command is: `stat -f %m notes.txt`", "stat -f %m notes.txt", ""},
		{"command sentence not at the end is ignored", "The command is: `ls`\n\nBut wait, that's wrong.", "", ""},
	} {
		cmd, expl, ok := answerFromMarkup(tc.in)
		if cmd != tc.cmd || expl != tc.expl || ok != (tc.cmd != "") {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q)", tc.name, cmd, expl, ok, tc.cmd, tc.expl)
		}
	}
}

type namedModel struct {
	fakeModel
	provider string
}

func (m *namedModel) Provider() string { return m.provider }

func TestReasoningOptions(t *testing.T) {
	for _, tc := range []struct {
		provider, effort string
		want             bool
	}{
		{"ollama", "none", true},
		{"openai-compat", "low", true},
		{"ollama", "", false},
		{"anthropic", "none", false},
		{"openai", "none", false},
	} {
		po := reasoningOptions(&namedModel{provider: tc.provider}, tc.effort)
		if (po != nil) != tc.want {
			t.Errorf("%s/%q: got options %v, want present=%v", tc.provider, tc.effort, po, tc.want)
			continue
		}
		if po != nil {
			o, ok := po[tc.provider].(*openaicompat.ProviderOptions)
			if !ok || o.ReasoningEffort == nil || string(*o.ReasoningEffort) != tc.effort {
				t.Errorf("%s: options not set for provider key: %#v", tc.provider, po)
			}
		}
	}
}

func TestReasoningEffortReachesModelCall(t *testing.T) {
	m := &namedModel{provider: "ollama"}
	m.steps = []step{{text: "ls -la"}}
	a := &Assistant{Model: m, Man: &fakeMan{}, NoTools: true, ReasoningEffort: "none"}
	if _, err := a.Ask(context.Background(), Request{Question: "list files"}); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) == 0 || m.calls[0].ProviderOptions["ollama"] == nil {
		t.Fatalf("reasoning effort not passed to the model call: %+v", m.calls)
	}
}

func TestEmptyReplyRetriesWithoutReasoningEffort(t *testing.T) {
	m := &namedModel{provider: "ollama"}
	m.steps = []step{{text: ""}, {text: "stat -f %z notes.txt"}}
	a := &Assistant{Model: m, Man: &fakeMan{}, ReasoningEffort: "none"}
	res, err := a.Ask(context.Background(), Request{Question: "size of notes.txt"})
	if err != nil {
		t.Fatalf("Ask: %v (calls: %d)", err, len(m.calls))
	}
	if res.Command != "stat -f %z notes.txt" {
		t.Fatalf("command %q", res.Command)
	}
	if len(m.calls) != 2 || m.calls[0].ProviderOptions["ollama"] == nil || m.calls[1].ProviderOptions["ollama"] != nil {
		t.Fatalf("want first call with effort, retry without: %+v", m.calls)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[len(res.Notes)-1], "retried") {
		t.Errorf("notes: %v", res.Notes)
	}
	if a.ReasoningEffort != "none" {
		t.Errorf("Ask mutated the caller's Assistant: %q", a.ReasoningEffort)
	}
}

func TestEmptyReplyWithoutReasoningEffortIsNotRetried(t *testing.T) {
	m := &namedModel{provider: "ollama"}
	m.steps = []step{{text: ""}, {text: "ls"}}
	a := &Assistant{Model: m, Man: &fakeMan{}}
	if _, err := a.Ask(context.Background(), Request{Question: "q"}); !errors.Is(err, ErrNoCommand) {
		t.Fatalf("want ErrNoCommand, got %v", err)
	}
	if len(m.calls) != 1 {
		t.Fatalf("calls: %d", len(m.calls))
	}
}
