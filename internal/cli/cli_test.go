package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecf/heyman/internal/assist"
	"github.com/alecf/heyman/internal/config"
	"github.com/alecf/heyman/internal/llm"
	"github.com/spf13/cobra"
)

// parseArgv runs argv through cobra (with the real flag setup) and
// parseRequest, without calling a model.
func parseArgv(t *testing.T, argv ...string) (assist.Request, *rootFlags, error) {
	t.Helper()
	var (
		req   assist.Request
		flags *rootFlags
	)
	root := newRootCmd("test", "", "", func(cmd *cobra.Command, f *rootFlags, args []string) error {
		flags = f
		var err error
		req, err = parseRequest(args, cmd.ArgsLenAtDash(), f.section)
		return err
	})
	root.SetArgs(argv)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	return req, flags, err
}

func TestParseArgv(t *testing.T) {
	tests := []struct {
		argv             []string
		cmd, sec, q      string
		explain, wantErr bool
		model            string
	}{
		{argv: []string{"ls", "sort", "by", "size"}, cmd: "ls", q: "sort by size"},
		{argv: []string{"ls", "-la", "what", "does", "this", "do"}, cmd: "ls", q: "-la what does this do"},
		{argv: []string{"3", "printf", "what"}, cmd: "printf", sec: "3", q: "what"},
		{argv: []string{"-s", "3", "printf", "what"}, cmd: "printf", sec: "3", q: "what"},
		{argv: []string{"--section=8", "mount", "nfs"}, cmd: "mount", sec: "8", q: "nfs"},
		{argv: []string{"git", "--", "--since", "what"}, cmd: "git", q: "--since what"},
		{argv: []string{"3", "printf", "--", "-x"}, cmd: "printf", sec: "3", q: "-x"},
		{argv: []string{"git", "what", "does", "--", "mean"}, cmd: "git", q: "what does -- mean"},
		{argv: []string{"--", "list", "big", "files"}, q: "list big files"},
		{argv: []string{"-e", "--", "list", "-big", "files"}, q: "list -big files", explain: true},
		{argv: []string{"-m", "x", "--", "q"}, q: "q", model: "x"},
		{argv: []string{"-m", "ollama/qwen3:4b", "-e", "find", "go", "files"}, cmd: "find", q: "go files", explain: true, model: "ollama/qwen3:4b"},
		{argv: []string{"list all big files"}, q: "list all big files"},
		{argv: []string{"  spaced  request "}, q: "spaced  request"},
		{argv: []string{}, wantErr: true},
		{argv: []string{"--"}, wantErr: true},
		{argv: []string{"--", ""}, wantErr: true},
		{argv: []string{"--", " "}, wantErr: true},
		{argv: []string{"ls"}, wantErr: true},
		{argv: []string{"ls", "--"}, wantErr: true},
		{argv: []string{"-s", "3", "printf"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.argv, " "), func(t *testing.T) {
			req, f, err := parseArgv(t, tt.argv...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if req.Command != tt.cmd || req.Section != tt.sec || req.Question != tt.q {
				t.Errorf("req = %+v, want cmd=%q sec=%q q=%q", req, tt.cmd, tt.sec, tt.q)
			}
			if f.explain != tt.explain || f.model != tt.model {
				t.Errorf("flags explain=%v model=%q", f.explain, f.model)
			}
		})
	}
}

func TestSubcommandsRoute(t *testing.T) {
	t.Setenv("HEYMAN_CONFIG", filepath.Join(t.TempDir(), "c.toml"))
	t.Setenv("HEYMAN_CACHE_DIR", t.TempDir())
	for _, argv := range [][]string{{"profile", "list"}, {"cache-stats"}, {"clear-cache"}, {"profile", "show", "nope"}} {
		called := false
		root := newRootCmd("test", "", "", func(*cobra.Command, *rootFlags, []string) error {
			called = true
			return nil
		})
		root.SetArgs(argv)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		_ = captureStdout(t, func() { _ = root.Execute() })
		if called {
			t.Errorf("%v routed to the root command", argv)
		}
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

func TestResolveModel(t *testing.T) {
	cfg := &config.Config{DefaultProfile: "ollama-gemma3n", Profiles: map[string]config.Profile{}}
	cfg.AddProfile("gemma3n", config.Profile{Provider: "ollama", Model: "gemma3n:e2b", BaseURL: "http://h:1/v1"})
	cfg.AddProfile("claude", config.Profile{Provider: "anthropic", Model: "claude-haiku-4-5"})
	cfg.AddProfile("broken", config.Profile{Provider: "ollama"})
	t.Setenv("HEYMAN_CONFIG", "/x/config.toml")

	tests := []struct {
		name                       string
		flags                      rootFlags
		envModel, envProfile       string
		defaultProfile             string
		model, base, source, errIn string
	}{
		{name: "flag model wins", flags: rootFlags{model: "x/y", profile: "claude"}, envModel: "a/b", model: "x/y", source: "--model"},
		{name: "flag profile beats env model", flags: rootFlags{profile: "gemma3n"}, envModel: "a/b", model: "ollama/gemma3n:e2b", base: "http://h:1/v1", source: "profile gemma3n"},
		{name: "env model", envModel: "a/b", envProfile: "claude", model: "a/b", source: "HEYMAN_MODEL"},
		{name: "env profile", envProfile: "claude", model: "anthropic/claude-haiku-4-5", source: "profile claude"},
		{name: "default profile", defaultProfile: "claude", model: "anthropic/claude-haiku-4-5", source: "profile claude"},
		{name: "no config", defaultProfile: "-", model: llm.DefaultModel, source: "default"},
		{name: "missing default profile", errIn: "set-default"},
		{name: "missing flag profile", flags: rootFlags{profile: "nope"}, errIn: "--profile"},
		{name: "missing env profile", envProfile: "nope", errIn: "HEYMAN_PROFILE"},
		{name: "invalid profile", flags: rootFlags{profile: "broken"}, errIn: "needs both provider and model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HEYMAN_MODEL", tt.envModel)
			t.Setenv("HEYMAN_PROFILE", tt.envProfile)
			c := *cfg
			switch tt.defaultProfile {
			case "":
			case "-":
				c.DefaultProfile = ""
			default:
				c.DefaultProfile = tt.defaultProfile
			}
			model, base, source, err := resolveModel(&tt.flags, &c)
			if tt.errIn != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errIn) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.errIn)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if model != tt.model || base != tt.base || source != tt.source {
				t.Errorf("got %q %q %q", model, base, source)
			}
		})
	}
}

func TestGenerateProfileName(t *testing.T) {
	cfg := &config.Config{}
	tests := []struct{ provider, model, want string }{
		{"ollama", "qwen3:4b", "qwen3"},
		{"openrouter", "moonshotai/kimi-k2", "kimi-k2"},
		{"openrouter", "moonshotai/kimi-k2:free", "kimi-k2"},
		{"ollama", "hf.co/unsloth/Qwen3-4B-GGUF:Q4_K_M", "qwen3-4b-gguf"},
		{"anthropic", "claude-haiku-4-5", "claude-haiku-4-5"},
		{"ollama", "llama3.2:1b-instruct-q4_0", "llama3-2"},
		{"ollama", ":latest", "ollama"},
		{"openai-compat", "///", "openai-compat"},
	}
	for _, tt := range tests {
		if got := generateProfileName(cfg, tt.provider, tt.model); got != tt.want {
			t.Errorf("generateProfileName(%q, %q) = %q, want %q", tt.provider, tt.model, got, tt.want)
		}
	}

	// Collision avoidance: base -> base-size -> provider-base -> provider-base-size -> base-N.
	cfg.AddProfile("llama3-2", config.Profile{})
	if got := generateProfileName(cfg, "ollama", "llama3.2:1b"); got != "llama3-2-1b" {
		t.Errorf("collision 1 = %q", got)
	}
	cfg.AddProfile("llama3-2-1b", config.Profile{})
	if got := generateProfileName(cfg, "ollama", "llama3.2:1b"); got != "ollama-llama3-2" {
		t.Errorf("collision 2 = %q", got)
	}
	cfg.AddProfile("ollama-llama3-2", config.Profile{})
	cfg.AddProfile("ollama-llama3-2-1b", config.Profile{})
	if got := generateProfileName(cfg, "ollama", "llama3.2:1b"); got != "llama3-2-2" {
		t.Errorf("collision 3 = %q", got)
	}
}

func TestExtractSizeSuffix(t *testing.T) {
	tests := map[string]string{"": "", "latest": "", "1b-instruct-q4_0": "1b", "70B": "70b", "e2b": "", "instruct-8b": "8b", "q8_0": ""}
	for in, want := range tests {
		if got := extractSizeSuffix(in); got != want {
			t.Errorf("extractSizeSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeProfileName(t *testing.T) {
	tests := map[string]string{"Qwen3 4B": "qwen3-4b", "--a//b--": "a-b", "a.b:c": "a-b-c", "ok_name": "ok_name", "": ""}
	for in, want := range tests {
		if got := sanitizeProfileName(in); got != want {
			t.Errorf("sanitizeProfileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckProfile(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	ctx := t.Context()
	if err := checkProfile(ctx, config.Profile{Provider: "claude-code", Model: "haiku"}); err == nil || !strings.Contains(err.Error(), "claude CLI") {
		t.Errorf("claude-code without claude on PATH: %v", err)
	}
	if err := checkProfile(ctx, config.Profile{Provider: "anthropic", Model: "claude-haiku-4-5"}); err == nil {
		t.Error("anthropic without key: want error")
	}
	if err := checkProfile(ctx, config.Profile{Provider: "ollama"}); err == nil {
		t.Error("profile without model: want error")
	}
	if err := checkProfile(ctx, config.Profile{Provider: "ollama", Model: "x:1", BaseURL: "http://127.0.0.1:1/v1"}); err == nil || !strings.Contains(err.Error(), "can't reach Ollama") {
		t.Errorf("unreachable ollama: %v", err)
	}
}

func TestOutputResultCached(t *testing.T) {
	res := &assist.Result{Command: "ls", Cached: true, Usage: assist.Usage{InputTokens: 1000, OutputTokens: 100}}
	spec := llm.Spec{Provider: "anthropic", Model: "claude-haiku-4-5"}
	out := captureStdout(t, func() {
		if err := outputResult(&rootFlags{json: true}, spec, res, false); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, `"cost": 0`) || !strings.Contains(out, `"cached": true`) {
		t.Errorf("cached JSON = %s", out)
	}
	out = captureStdout(t, func() { _ = outputResult(&rootFlags{}, spec, res, false) })
	if out != "ls\n" {
		t.Errorf("stdout = %q, want only the command", out)
	}
}

func TestErrNoRequestIsSentinel(t *testing.T) {
	_, err := parseRequest(nil, -1, "")
	if !errors.Is(err, errNoRequest) {
		t.Error(err)
	}
}
