package eval

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Rate is a pass count over n scored attempts.
type Rate struct{ Pass, N int }

func (r Rate) String() string {
	if r.N == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(r.Pass)/float64(r.N), r.Pass, r.N)
}

func (r *Rate) add(pass bool) {
	r.N++
	if pass {
		r.Pass++
	}
}

// ModelSummary aggregates one model's attempts.
type ModelSummary struct {
	Model   string
	Overall Rate
	ByDiff  map[string]*Rate
	ByTag   map[string]*Rate
	ByForm  map[string]*Rate

	Errors  int
	Skipped int

	LatencyAvg, LatencyP50, LatencyP95 float64 // seconds
	AvgToolCalls                       float64
	ConsultedExtraPct                  float64

	CostKnown   bool
	CostTotal   float64
	CostPerCase float64

	ExecRan, ExecPass, ExecSkipped, ExecRefError int

	Judged, JudgeCorrect, Disagree int
}

// Report is everything printed at the end of a run.
type Report struct {
	Models   []string
	Cases    []*Case
	Summary  []*ModelSummary
	Attempts []Attempt
	Header   string // free-form run description
}

// SummaryTags are always shown as pass-rate columns.
var SummaryTags = []string{"bsd-gnu", "multi-tool"}

// BuildReport aggregates attempts.
func BuildReport(models []string, cases []*Case, attempts []Attempt) *Report {
	r := &Report{Models: models, Cases: cases, Attempts: attempts}
	for _, m := range models {
		s := &ModelSummary{Model: m, ByDiff: map[string]*Rate{}, ByTag: map[string]*Rate{}, ByForm: map[string]*Rate{}}
		var lat []float64
		tools, extra := 0, 0
		allCostKnown := true
		for i := range attempts {
			a := &attempts[i]
			if a.Model != m {
				continue
			}
			if !a.Scored() {
				s.Skipped++
				continue
			}
			s.Overall.add(a.Pass)
			if a.Difficulty != "" {
				rateFor(s.ByDiff, a.Difficulty).add(a.Pass)
			}
			for _, t := range a.Tags {
				if !slices.Contains(Difficulties, t) {
					rateFor(s.ByTag, t).add(a.Pass)
				}
			}
			rateFor(s.ByForm, a.Form).add(a.Pass)
			if a.Error != "" {
				s.Errors++
			}
			lat = append(lat, float64(a.LatencyMS)/1000)
			tools += a.NonAnswerToolCalls()
			if a.ConsultedExtra {
				extra++
			}
			if a.CostUSD != nil {
				s.CostTotal += *a.CostUSD
			} else {
				allCostKnown = false
			}
			if a.Exec != nil {
				switch a.Exec.Status {
				case ExecPass:
					s.ExecRan++
					s.ExecPass++
				case ExecFail:
					s.ExecRan++
				case ExecSkipped:
					s.ExecSkipped++
				case ExecRefError:
					s.ExecRefError++
				}
			}
			if a.Judge != nil && a.Judge.Verdict != "" {
				s.Judged++
				if a.Judge.Correct() {
					s.JudgeCorrect++
				}
				if a.Judge.Correct() != a.Pass {
					s.Disagree++
				}
			}
		}
		n := s.Overall.N
		if n > 0 {
			s.LatencyAvg = mean(lat)
			s.LatencyP50 = percentile(lat, 50)
			s.LatencyP95 = percentile(lat, 95)
			s.AvgToolCalls = float64(tools) / float64(n)
			s.ConsultedExtraPct = 100 * float64(extra) / float64(n)
			s.CostPerCase = s.CostTotal / float64(n)
		}
		s.CostKnown = allCostKnown
		r.Summary = append(r.Summary, s)
	}
	return r
}

func rateFor(m map[string]*Rate, k string) *Rate {
	if m[k] == nil {
		m[k] = &Rate{}
	}
	return m[k]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// percentile uses nearest-rank on a sorted copy.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[max(0, min(rank-1, len(s)-1))]
}

// --- rendering ---

// table renders an aligned Markdown table (readable as plain text too).
func table(header []string, rows [][]string) string {
	w := make([]int, len(header))
	for i, h := range header {
		w[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			w[i] = max(w[i], utf8.RuneCountInString(c))
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for i, c := range cells {
			b.WriteString(" " + c + strings.Repeat(" ", w[i]-utf8.RuneCountInString(c)) + " |")
		}
		b.WriteString("\n")
	}
	line(header)
	b.WriteString("|")
	for i := range header {
		b.WriteString(strings.Repeat("-", w[i]+2) + "|")
	}
	b.WriteString("\n")
	for _, r := range rows {
		line(r)
	}
	return b.String()
}

func money(known bool, v float64) string {
	if !known {
		return fmt.Sprintf("≥$%.4f", v)
	}
	return fmt.Sprintf("$%.4f", v)
}

func rateOr(m map[string]*Rate, k string) string {
	if r := m[k]; r != nil {
		return r.String()
	}
	return "–"
}

// Markdown renders the full report.
func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# heyman eval results\n\n")
	if r.Header != "" {
		b.WriteString(r.Header + "\n\n")
	}

	b.WriteString("## Summary\n\n")
	hdr := []string{"model", "pass", "easy", "medium", "hard", "bsd-gnu", "multi-tool", "cmd form", "-- form", "errors"}
	var rows [][]string
	for _, s := range r.Summary {
		rows = append(rows, []string{s.Model, s.Overall.String(),
			rateOr(s.ByDiff, "easy"), rateOr(s.ByDiff, "medium"), rateOr(s.ByDiff, "hard"),
			rateOr(s.ByTag, "bsd-gnu"), rateOr(s.ByTag, "multi-tool"),
			rateOr(s.ByForm, "command"), rateOr(s.ByForm, "--"), fmt.Sprint(s.Errors)})
	}
	b.WriteString(table(hdr, rows) + "\n")

	hdr = []string{"model", "lat avg", "p50", "p95", "tool calls", "read extra man", "cost total", "cost/case", "exec pass", "judge ok", "judge≠checks", "skipped"}
	rows = nil
	for _, s := range r.Summary {
		execs := "–"
		if s.ExecRan+s.ExecSkipped+s.ExecRefError > 0 {
			execs = fmt.Sprintf("%d/%d", s.ExecPass, s.ExecRan)
			if s.ExecSkipped > 0 {
				execs += fmt.Sprintf(" (%d skip)", s.ExecSkipped)
			}
			if s.ExecRefError > 0 {
				execs += fmt.Sprintf(" (%d ref err)", s.ExecRefError)
			}
		}
		judge, dis := "–", "–"
		if s.Judged > 0 {
			judge = Rate{s.JudgeCorrect, s.Judged}.String()
			dis = fmt.Sprint(s.Disagree)
		}
		rows = append(rows, []string{s.Model,
			fmt.Sprintf("%.1fs", s.LatencyAvg), fmt.Sprintf("%.1fs", s.LatencyP50), fmt.Sprintf("%.1fs", s.LatencyP95),
			fmt.Sprintf("%.1f", s.AvgToolCalls), fmt.Sprintf("%.0f%%", s.ConsultedExtraPct),
			money(s.CostKnown, s.CostTotal), money(s.CostKnown, s.CostPerCase), execs, judge, dis, fmt.Sprint(s.Skipped)})
	}
	b.WriteString(table(hdr, rows) + "\n")
	b.WriteString("_tool calls_ excludes the final `answer`; _read extra man_ = attempts that read a man page beyond the preloaded one; _exec pass_ = passed/ran.\n\n")

	// Pass rate by tag.
	tagSet := map[string]bool{}
	for _, s := range r.Summary {
		for t := range s.ByTag {
			tagSet[t] = true
		}
	}
	tags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	if len(tags) > 0 {
		b.WriteString("## Pass rate by tag\n\n")
		hdr = append([]string{"tag"}, r.Models...)
		rows = nil
		for _, t := range tags {
			row := []string{t}
			for _, s := range r.Summary {
				row = append(row, rateOr(s.ByTag, t))
			}
			rows = append(rows, row)
		}
		b.WriteString(table(hdr, rows) + "\n")
	}

	b.WriteString(r.matrix())
	b.WriteString(r.failures())
	b.WriteString(r.disagreements())
	return b.String()
}

func (r *Report) byModelCase() map[string]map[string][]*Attempt {
	m := map[string]map[string][]*Attempt{}
	for i := range r.Attempts {
		a := &r.Attempts[i]
		if m[a.Model] == nil {
			m[a.Model] = map[string][]*Attempt{}
		}
		m[a.Model][a.CaseID] = append(m[a.Model][a.CaseID], a)
	}
	return m
}

func (r *Report) matrix() string {
	idx := r.byModelCase()
	var b strings.Builder
	b.WriteString("## Per-case results\n\n")
	b.WriteString("✓ pass · ✗ fail · E provider error · – skipped · `*` judge disagrees · k/n with --repeat\n\n")
	hdr := []string{"case", "diff"}
	for i := range r.Models {
		hdr = append(hdr, fmt.Sprintf("m%d", i+1))
	}
	var rows [][]string
	for _, c := range r.Cases {
		row := []string{c.ID, c.Difficulty()}
		any := false
		for _, m := range r.Models {
			as := idx[m][c.ID]
			if len(as) > 0 {
				any = true
			}
			row = append(row, cell(as))
		}
		if any {
			rows = append(rows, row)
		}
	}
	b.WriteString(table(hdr, rows) + "\n")
	for i, m := range r.Models {
		fmt.Fprintf(&b, "m%d = %s  ", i+1, m)
	}
	b.WriteString("\n\n")
	return b.String()
}

func cell(as []*Attempt) string {
	if len(as) == 0 {
		return ""
	}
	pass, n, errs, dis := 0, 0, 0, false
	for _, a := range as {
		if !a.Scored() {
			continue
		}
		n++
		if a.Pass {
			pass++
		}
		if a.Error != "" {
			errs++
		}
		if a.Judge != nil && a.Judge.Verdict != "" && a.Judge.Correct() != a.Pass {
			dis = true
		}
	}
	var s string
	switch {
	case n == 0:
		s = "–"
	case n == 1 && pass == 1:
		s = "✓"
	case n == 1 && errs == 1:
		s = "E"
	case n == 1:
		s = "✗"
	default:
		s = fmt.Sprintf("%d/%d", pass, n)
	}
	if dis {
		s += "*"
	}
	return s
}

func (r *Report) caseByID() map[string]*Case {
	m := map[string]*Case{}
	for _, c := range r.Cases {
		m[c.ID] = c
	}
	return m
}

func (r *Report) failures() string {
	cases := r.caseByID()
	var b strings.Builder
	n := 0
	for i := range r.Attempts {
		a := &r.Attempts[i]
		if !a.Scored() || a.Pass {
			continue
		}
		if n == 0 {
			b.WriteString("## Failures\n\n```\n")
		}
		n++
		writeAttempt(&b, a, cases[a.CaseID])
	}
	if n > 0 {
		b.WriteString("```\n\n")
	}
	return b.String()
}

func (r *Report) disagreements() string {
	cases := r.caseByID()
	var b strings.Builder
	n := 0
	for i := range r.Attempts {
		a := &r.Attempts[i]
		if !a.Scored() || a.Judge == nil || a.Judge.Verdict == "" || a.Judge.Correct() == a.Pass {
			continue
		}
		if n == 0 {
			b.WriteString("## Checks vs judge disagreements\n\n")
			b.WriteString("Candidates the deterministic checks and the LLM judge graded differently. Checks-fail/judge-correct often means a brittle check or missing reference; checks-pass/judge-incorrect often means a check that is too lenient (or a judge mistake).\n\n```\n")
		}
		n++
		writeAttempt(&b, a, cases[a.CaseID])
	}
	if n > 0 {
		b.WriteString("```\n\n")
	}
	return b.String()
}

func writeAttempt(b *strings.Builder, a *Attempt, c *Case) {
	mark := "✗"
	if a.Pass {
		mark = "✓"
	}
	rep := ""
	if a.Repeat > 1 {
		rep = fmt.Sprintf(" [r%d]", a.Repeat)
	}
	fmt.Fprintf(b, "%s %s · %s%s\n", mark, a.Model, a.CaseID, rep)
	if c != nil {
		form := "heyman -- "
		if c.Command != "" {
			form = "heyman " + c.Command + " "
		}
		fmt.Fprintf(b, "    ask:   %s%s\n", form, c.Question)
	}
	if a.Command != "" {
		fmt.Fprintf(b, "    got:   %s\n", oneLine(a.Command))
	}
	if c != nil && len(c.Reference) > 0 {
		fmt.Fprintf(b, "    ref:   %s\n", oneLine(c.Reference[0]))
	}
	if a.Error != "" {
		fmt.Fprintf(b, "    error: %s\n", truncate(oneLine(a.Error), 300))
	}
	if !a.Pass && a.Grade.FirstFailure != "" {
		fmt.Fprintf(b, "    why:   %s\n", truncate(a.Grade.FirstFailure, 300))
	}
	if a.Exec != nil && a.Exec.Status != ExecPass {
		fmt.Fprintf(b, "    exec:  %s %s\n", a.Exec.Status, truncate(oneLine(a.Exec.Reason), 200))
	}
	if a.Judge != nil {
		if a.Judge.Error != "" {
			fmt.Fprintf(b, "    judge: error %s\n", truncate(oneLine(a.Judge.Error), 200))
		} else {
			fmt.Fprintf(b, "    judge: %s — %s\n", a.Judge.Verdict, truncate(oneLine(a.Judge.Reason), 400))
		}
	}
	if len(a.ManPages) > 0 {
		fmt.Fprintf(b, "    man:   %s\n", strings.Join(a.ManPages, ", "))
	}
	b.WriteString("\n")
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
