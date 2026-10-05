// Package eval loads heyman evaluation cases, grades candidate commands
// against them, runs models over the suite, and reports results.
package eval

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Suite is a parsed case file.
type Suite struct {
	Version int    `yaml:"version"`
	Cases   []Case `yaml:"cases"`
}

// Case is one natural-language request with its grading rules.
type Case struct {
	ID        string   `yaml:"id"`
	Command   string   `yaml:"command,omitempty"`
	Section   string   `yaml:"section,omitempty"`
	Question  string   `yaml:"question"`
	Tags      []string `yaml:"tags"`
	Reference []string `yaml:"reference"`
	// ReferencesByOS optionally overrides Reference for one GOOS (e.g. GNU
	// forms for linux). Used for the judge prompt; exec always uses
	// Reference[0], which is portable.
	ReferencesByOS map[string][]string `yaml:"references_by_os,omitempty"`
	Checks         []Check             `yaml:"checks"`
	Forbid         []string            `yaml:"forbid,omitempty"`
	Platforms      map[string][]Check  `yaml:"platforms,omitempty"`
	Exec           *ExecSpec           `yaml:"exec,omitempty"`

	forbid []*regexp.Regexp
}

// ExecSpec describes an execution-based check.
type ExecSpec struct {
	Setup          string `yaml:"setup"`
	Compare        string `yaml:"compare"`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty"`
}

// Compare modes for ExecSpec.Compare.
const (
	CompareStdout       = "stdout"
	CompareStdoutSorted = "stdout_sorted"
	CompareExit         = "exit"
)

// ReferencesFor returns the references valid on goos.
func (c *Case) ReferencesFor(goos string) []string {
	if refs := c.ReferencesByOS[goos]; len(refs) > 0 {
		return refs
	}
	return c.Reference
}

// Difficulty levels (exactly one is expected in each case's tags).
var Difficulties = []string{"easy", "medium", "hard"}

// HasTag reports whether the case carries tag.
func (c *Case) HasTag(tag string) bool { return slices.Contains(c.Tags, tag) }

// Difficulty returns the case's difficulty tag, or "".
func (c *Case) Difficulty() string {
	for _, d := range Difficulties {
		if c.HasTag(d) {
			return d
		}
	}
	return ""
}

// Form is "command" when the case names a command (its page is preloaded)
// and "--" otherwise.
func (c *Case) Form() string {
	if c.Command != "" {
		return "command"
	}
	return "--"
}

// Check is one deterministic check. Exactly one of Regex, Program, Any is set.
type Check struct {
	Regex   string
	Program string
	Any     []Check

	re *regexp.Regexp
}

// String renders the check compactly for reports.
func (c Check) String() string {
	switch {
	case c.Regex != "":
		return "regex: " + c.Regex
	case c.Program != "":
		return "program: " + c.Program
	default:
		parts := make([]string, len(c.Any))
		for i, sub := range c.Any {
			parts[i] = "{" + sub.String() + "}"
		}
		return "any: [" + strings.Join(parts, ", ") + "]"
	}
}

// UnmarshalYAML decodes a single-key mapping ({regex|program|any: …}).
func (c *Check) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: check must be a mapping with one key (regex, program or any)", n.Line)
	}
	if len(n.Content) != 2 {
		return fmt.Errorf("line %d: check must have exactly one key (regex, program or any), got %d", n.Line, len(n.Content)/2)
	}
	key, val := n.Content[0].Value, n.Content[1]
	switch key {
	case "regex", "program":
		if val.Kind != yaml.ScalarNode || val.Value == "" {
			return fmt.Errorf("line %d: %s must be a non-empty string", val.Line, key)
		}
		if key == "regex" {
			c.Regex = val.Value
		} else {
			c.Program = val.Value
		}
	case "any":
		if val.Kind != yaml.SequenceNode || len(val.Content) == 0 {
			return fmt.Errorf("line %d: any must be a non-empty list of checks", val.Line)
		}
		for _, item := range val.Content {
			var sub Check
			if err := sub.UnmarshalYAML(item); err != nil {
				return err
			}
			c.Any = append(c.Any, sub)
		}
	default:
		return fmt.Errorf("line %d: unknown check key %q (want regex, program or any)", n.Content[0].Line, key)
	}
	return nil
}

func (c *Check) compile() error {
	if c.Regex != "" {
		re, err := regexp.Compile(c.Regex)
		if err != nil {
			return fmt.Errorf("bad regex %q: %w", c.Regex, err)
		}
		c.re = re
	}
	for i := range c.Any {
		if err := c.Any[i].compile(); err != nil {
			return err
		}
	}
	return nil
}

// LoadFile reads and validates a case file.
func LoadFile(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse decodes and validates a case file. Unknown keys are errors.
func Parse(data []byte) (*Suite, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Suite
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func (s *Suite) validate() error {
	if s.Version != 1 {
		return fmt.Errorf("unsupported version %d (want 1)", s.Version)
	}
	if len(s.Cases) == 0 {
		return errors.New("no cases")
	}
	var errs []error
	seen := map[string]bool{}
	for i := range s.Cases {
		c := &s.Cases[i]
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("case %q: %s", c.ID, fmt.Sprintf(format, args...)))
		}
		if !validID.MatchString(c.ID) {
			fail("invalid or missing id")
		}
		if seen[c.ID] {
			fail("duplicate id")
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Question) == "" {
			fail("missing question")
		}
		if c.Section != "" && c.Command == "" {
			fail("section without command")
		}
		if len(c.Reference) == 0 {
			fail("needs at least one reference")
		}
		if len(c.Checks) == 0 {
			fail("needs at least one check")
		}
		n := 0
		for _, d := range Difficulties {
			if c.HasTag(d) {
				n++
			}
		}
		if n != 1 {
			fail("needs exactly one difficulty tag (easy|medium|hard), has %d", n)
		}
		for j := range c.Checks {
			if err := c.Checks[j].compile(); err != nil {
				fail("%v", err)
			}
		}
		for goos := range c.ReferencesByOS {
			if goos != "darwin" && goos != "linux" {
				fail("unknown references_by_os platform %q", goos)
			}
		}
		for goos, checks := range c.Platforms {
			if goos != "darwin" && goos != "linux" {
				fail("unknown platform %q", goos)
			}
			for j := range checks {
				if err := checks[j].compile(); err != nil {
					fail("platform %s: %v", goos, err)
				}
			}
		}
		c.forbid = nil
		for _, f := range c.Forbid {
			re, err := regexp.Compile(f)
			if err != nil {
				fail("bad forbid regex %q: %v", f, err)
				continue
			}
			c.forbid = append(c.forbid, re)
		}
		if c.Exec != nil {
			switch c.Exec.Compare {
			case CompareStdout, CompareStdoutSorted, CompareExit:
			default:
				fail("exec.compare must be stdout, stdout_sorted or exit, got %q", c.Exec.Compare)
			}
			if c.Exec.TimeoutSeconds < 0 {
				fail("exec.timeout_seconds must be >= 0")
			}
		}
	}
	return errors.Join(errs...)
}

// Filter returns cases whose id or any tag matches re (nil keeps all).
func (s *Suite) Filter(re *regexp.Regexp) []*Case {
	var out []*Case
	for i := range s.Cases {
		c := &s.Cases[i]
		if re == nil || re.MatchString(c.ID) || slices.ContainsFunc(c.Tags, re.MatchString) {
			out = append(out, c)
		}
	}
	return out
}
