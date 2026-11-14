package assist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/alecf/heyman/internal/manpage"
)

// AppleFM answers requests with Apple's on-device Foundation Model through
// the `fm` CLI that ships with macOS 27 (`fm respond`). The model has a
// 4,096-token context window and `fm respond` has no way to register custom
// tools, so this backend is single-shot: the named command's man page is
// trimmed to fit and preloaded, and the answer comes back as JSON constrained
// by a generation schema.
type AppleFM struct {
	// Model is passed to `fm respond --model` ("system").
	Model string
	// Binary defaults to "fm" on PATH.
	Binary string
	// PreloadChars caps the preloaded man page. Default 9000 (~2,500
	// tokens), which leaves room for the instructions and the answer. If the
	// prompt still overflows the context window, Ask retries with half as
	// much.
	PreloadChars int
	Man          ManSource
	OnEvent      func(Event)
}

// appleDefaultPreload is sized for the 4,096-token context window: man pages
// run about 3.7 characters per token with Apple's tokenizer.
const appleDefaultPreload = 9000

// appleMinPreload is the smallest preload worth retrying with.
const appleMinPreload = 2000

// errAppleContext matches fm's error when the prompt doesn't fit.
var errAppleContext = errors.New("prompt exceeded the on-device model's context window")

// Length caps for the generated fields. Without them the small model
// sometimes repeats itself until it runs out of context, which takes a
// minute or more.
const (
	appleMaxCommand     = 300
	appleMaxExplanation = 800
)

// appleTimeout bounds one `fm respond` call, as a backstop for runaway
// generation.
const appleTimeout = 45 * time.Second

// fmLicenseExit is fm's exit status when the machine hasn't agreed to the
// Foundation Models CLI terms.
const fmLicenseExit = 69

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// appleSchema is a generation schema for the answer. fm's inline schemas use
// the same shape `fm schema object` prints, including x-order.
func appleSchema(explain bool) string {
	props := map[string]any{
		"command": map[string]any{"type": "string", "maxLength": appleMaxCommand, "description": "The single shell command that does what the user asked, with no markdown"},
	}
	order := []string{"command"}
	if explain {
		props["explanation"] = map[string]any{"type": "string", "maxLength": appleMaxExplanation, "description": "A concise explanation of what the command does and why those flags"}
		order = append(order, "explanation")
	}
	b, _ := json.Marshal(map[string]any{
		"title":                "Answer",
		"type":                 "object",
		"additionalProperties": false,
		"required":             order,
		"x-order":              order,
		"properties":           props,
	})
	return string(b)
}

func appleArgs(model, system, schema string) []string {
	args := []string{"respond", "--no-stream", "--greedy", "--schema", schema, "--instructions", system}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// Ask implements Answerer.
func (a *AppleFM) Ask(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Question) == "" {
		return nil, fmt.Errorf("no question specified")
	}
	bin := a.Binary
	if bin == "" {
		bin = "fm"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("apple provider needs the `fm` CLI (macOS 27 or later): %w", err)
	}
	man := a.Man
	if man == nil {
		man = manpage.NewFetcher()
	}
	preloadChars := a.PreloadChars
	if preloadChars == 0 {
		preloadChars = appleDefaultPreload
	}

	req, preload, missing, err := preparePreload(man, req)
	if err != nil {
		return nil, err
	}
	model := a.Model
	if model == "" {
		model = "system"
	}
	res := &Result{Model: "apple/" + model}
	if req.Command != "" {
		res.ManPages = []string{pageKey(req.Command, req.Section)}
	}
	note := func(msg string) {
		res.Notes = append(res.Notes, msg)
		if a.OnEvent != nil {
			a.OnEvent(Event{Kind: "note", Detail: msg})
		}
	}
	if missing != "" {
		note(missingNote(missing))
	}

	schema := appleSchema(req.Explain)
	var out string
	for {
		res.Steps++
		system := systemPrompt(req, preload, preloadChars, false, missing)
		out, err = a.respond(ctx, bin, model, system, schema, req.Question)
		if !errors.Is(err, errAppleContext) || req.Command == "" || preloadChars/2 < appleMinPreload {
			break
		}
		preloadChars /= 2
		note(fmt.Sprintf("prompt too long for the on-device model; retried with %d characters of the man page", preloadChars))
	}
	if err != nil {
		return nil, err
	}

	var ans answerInput
	if json.Unmarshal([]byte(out), &ans) == nil {
		res.Command, res.Explanation = cleanCommand(ans.Command), strings.TrimSpace(ans.Explanation)
	} else {
		res.Command, res.Explanation = ParseText(out)
	}
	if !req.Explain {
		res.Explanation = ""
	}
	if res.Command == "" {
		res.RawText = out
		return res, ErrNoCommand
	}
	return res, nil
}

// respond runs one `fm respond` call and returns its stdout.
func (a *AppleFM) respond(ctx context.Context, bin, model, system, schema, question string) (string, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, appleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, appleArgs(model, system, schema)...)
	cmd.Stdin = strings.NewReader(question)
	cmd.Env = append(cmd.Environ(), "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()), nil
	}
	if parent.Err() != nil {
		return "", parent.Err()
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("the on-device model gave no answer within %s", appleTimeout)
	}
	msg := strings.TrimSpace(ansiEscape.ReplaceAllString(stderr.String(), ""))
	if msg == "" {
		msg = strings.TrimSpace(stdout.String())
	}
	msg = strings.TrimPrefix(msg, "Error: ")
	var exitErr *exec.ExitError
	switch {
	case strings.Contains(msg, "exceeded the model's context size"):
		return "", errAppleContext
	case errors.As(err, &exitErr) && exitErr.ExitCode() == fmLicenseExit:
		return "", fmt.Errorf("Apple Foundation Models needs the license accepted first: run `sudo fm license`")
	}
	return "", fmt.Errorf("fm respond failed: %w: %s", err, msg)
}
