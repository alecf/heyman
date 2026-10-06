package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SiteData is the eval snapshot published on the project page.
type SiteData struct {
	Generated string      `json:"generated"`
	Runs      []SiteRun   `json:"runs"`
	Models    []SiteModel `json:"models"`
	Cases     []SiteCase  `json:"cases"`
}

// SiteRun describes one results directory that went into the snapshot.
type SiteRun struct {
	Dir         string   `json:"dir"`
	OS          string   `json:"os"`
	Models      []string `json:"models"`
	GitHead     string   `json:"git_head"`
	Completed   int      `json:"completed"`
	Planned     int      `json:"planned"`
	Interrupted bool     `json:"interrupted"`
	Judge       string   `json:"judge,omitempty"`
	Hardware    Hardware `json:"hardware"`
	Ollama      string   `json:"ollama_version,omitempty"`
	Reasoning   string   `json:"reasoning_effort,omitempty"`
}

// SiteModel is one model's aggregate scores.
type SiteModel struct {
	Model        string              `json:"model"`
	Pass         int                 `json:"pass"`
	N            int                 `json:"n"`
	ByDifficulty map[string][2]int   `json:"by_difficulty"` // [pass, n]
	ByTag        map[string][2]int   `json:"by_tag"`
	ByForm       map[string][2]int   `json:"by_form"`
	Errors       int                 `json:"errors"`
	LatencyAvg   float64             `json:"latency_avg_s"`
	LatencyP50   float64             `json:"latency_p50_s"`
	LatencyP95   float64             `json:"latency_p95_s"`
	ToolCalls    float64             `json:"avg_tool_calls"`
	ExtraManPct  float64             `json:"read_extra_man_pct"`
	CostKnown    bool                `json:"cost_known"`
	CostPerCase  float64             `json:"cost_per_case_usd"`
	ExecPass     int                 `json:"exec_pass"`
	ExecRan      int                 `json:"exec_ran"`
	JudgeCorrect int                 `json:"judge_correct"`
	Judged       int                 `json:"judged"`
	Results      map[string]SiteCell `json:"results"` // by case id

	// Where it ran: the run, and for local models the peak memory, context
	// window and settings observed during the run.
	Run       string   `json:"run"`
	Local     bool     `json:"local"`
	MemoryGB  *float64 `json:"memory_gb,omitempty"`
	Context   int      `json:"context,omitempty"`
	Reasoning string   `json:"reasoning_effort,omitempty"`
}

// SiteCell is one model's answer to one case.
type SiteCell struct {
	Command      string `json:"command,omitempty"`
	Pass         bool   `json:"pass"`
	Error        string `json:"error,omitempty"`
	FirstFailure string `json:"first_failure,omitempty"`
	Judge        string `json:"judge,omitempty"`
	JudgeReason  string `json:"judge_reason,omitempty"`
}

// SiteCase is a case's public description.
type SiteCase struct {
	ID         string   `json:"id"`
	Command    string   `json:"command,omitempty"`
	Question   string   `json:"question"`
	Difficulty string   `json:"difficulty"`
	Tags       []string `json:"tags"`
	Reference  string   `json:"reference"`
}

// ReadAttempts loads results.jsonl from a results directory.
func ReadAttempts(dir string) ([]Attempt, error) {
	f, err := os.Open(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Attempt
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var a Attempt
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		out = append(out, a)
	}
	return out, sc.Err()
}

// Regrade re-applies the current deterministic checks to stored answers, so
// fixes to the case file apply to old runs without calling any model. Stored
// exec outcomes are kept (an exec failure still fails the attempt).
func Regrade(cases []*Case, attempts []Attempt, goos string) {
	byID := map[string]*Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	for i := range attempts {
		a := &attempts[i]
		c := byID[a.CaseID]
		if c == nil || a.Skipped != "" {
			continue
		}
		a.Grade = c.GradeCommand(a.Command, goos)
		if a.Error != "" && a.Command == "" {
			a.Grade.FirstFailure = "error: " + a.Error
		}
		a.Pass = a.Grade.Pass && a.Error == ""
		if a.Exec != nil && a.Exec.Status == ExecFail {
			if a.Pass {
				a.Grade.FirstFailure = "exec: " + a.Exec.Reason
			}
			a.Pass = false
		}
	}
}

// BuildSiteData merges result directories into a site snapshot. When the
// same model appears in several runs, the later directory wins per case.
func BuildSiteData(cases []*Case, dirs []string, goos, generated string) (*SiteData, error) {
	data := &SiteData{Generated: generated}
	var all []Attempt
	var models []string
	seen := map[string]bool{}
	runOf := map[string]modelRun{}
	for _, dir := range dirs {
		attempts, err := ReadAttempts(dir)
		if err != nil {
			return nil, err
		}
		var meta struct {
			OS          string                 `json:"os"`
			Models      []string               `json:"models"`
			GitHead     string                 `json:"git_head"`
			Completed   int                    `json:"completed"`
			Planned     int                    `json:"planned"`
			Interrupted bool                   `json:"interrupted"`
			Judge       string                 `json:"judge"`
			Hardware    Hardware               `json:"hardware"`
			Ollama      string                 `json:"ollama_version"`
			Reasoning   string                 `json:"reasoning_effort"`
			Local       map[string]LoadedModel `json:"local_models"`
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "run.json")); err == nil {
			_ = json.Unmarshal(raw, &meta)
		}
		data.Runs = append(data.Runs, SiteRun{
			Dir: filepath.Base(dir), OS: meta.OS, Models: meta.Models, GitHead: meta.GitHead,
			Completed: meta.Completed, Planned: meta.Planned, Interrupted: meta.Interrupted, Judge: meta.Judge,
			Hardware: meta.Hardware, Ollama: meta.Ollama, Reasoning: meta.Reasoning,
		})
		for _, m := range meta.Models {
			info := modelRun{run: filepath.Base(dir), reasoning: meta.Reasoning}
			if lm, ok := meta.Local[m]; ok {
				info.loaded = &lm
			}
			runOf[m] = info
		}
		for _, m := range meta.Models {
			if !seen[m] {
				seen[m] = true
				models = append(models, m)
			}
		}
		all = append(all, attempts...)
	}

	// Later runs override earlier ones for the same (model, case, repeat).
	type key struct {
		model, id string
		repeat    int
	}
	latest := map[key]Attempt{}
	var order []key
	for _, a := range all {
		k := key{a.Model, a.CaseID, a.Repeat}
		if _, ok := latest[k]; !ok {
			order = append(order, k)
		}
		latest[k] = a
	}
	merged := make([]Attempt, 0, len(order))
	for _, k := range order {
		merged = append(merged, latest[k])
	}
	Regrade(cases, merged, goos)

	report := BuildReport(models, cases, merged)
	for _, s := range report.Summary {
		m := SiteModel{
			Model: s.Model, Pass: s.Overall.Pass, N: s.Overall.N,
			ByDifficulty: rates(s.ByDiff), ByTag: rates(s.ByTag), ByForm: rates(s.ByForm),
			Errors: s.Errors, LatencyAvg: s.LatencyAvg, LatencyP50: s.LatencyP50, LatencyP95: s.LatencyP95,
			ToolCalls: s.AvgToolCalls, ExtraManPct: s.ConsultedExtraPct,
			CostKnown: s.CostKnown, CostPerCase: s.CostPerCase,
			ExecPass: s.ExecPass, ExecRan: s.ExecRan,
			JudgeCorrect: s.JudgeCorrect, Judged: s.Judged,
			Results: map[string]SiteCell{},
		}
		if info, ok := runOf[s.Model]; ok {
			m.Run = info.run
			m.Local = strings.HasPrefix(s.Model, "ollama/") || strings.HasPrefix(s.Model, "openai-compat/")
			if m.Local {
				m.Reasoning = info.reasoning
			}
			if info.loaded != nil {
				// Prefer measured runner RSS; Ollama's own figure can omit
				// memory-mapped weights.
				gb := info.loaded.MemoryGB
				if info.loaded.ResidentGB > 0 {
					gb = info.loaded.ResidentGB
				}
				m.MemoryGB, m.Context = &gb, info.loaded.Context
			}
		}
		data.Models = append(data.Models, m)
	}
	byModel := map[string]*SiteModel{}
	for i := range data.Models {
		byModel[data.Models[i].Model] = &data.Models[i]
	}
	for _, a := range merged {
		m := byModel[a.Model]
		if m == nil || a.Skipped != "" {
			continue
		}
		cell := SiteCell{Command: Normalize(a.Command), Pass: a.Pass, Error: a.Error, FirstFailure: a.Grade.FirstFailure}
		if a.Judge != nil {
			cell.Judge, cell.JudgeReason = a.Judge.Verdict, a.Judge.Reason
		}
		m.Results[a.CaseID] = cell
	}
	for _, c := range cases {
		ref := ""
		if refs := c.ReferencesFor(goos); len(refs) > 0 {
			ref = refs[0]
		}
		tags := append([]string(nil), c.Tags...)
		sort.Strings(tags)
		data.Cases = append(data.Cases, SiteCase{
			ID: c.ID, Command: c.Command, Question: c.Question,
			Difficulty: c.Difficulty(), Tags: tags, Reference: ref,
		})
	}
	return data, nil
}

type modelRun struct {
	run       string
	reasoning string
	loaded    *LoadedModel
}

func rates(m map[string]*Rate) map[string][2]int {
	out := make(map[string][2]int, len(m))
	for k, r := range m {
		out[k] = [2]int{r.Pass, r.N}
	}
	return out
}
