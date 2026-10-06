package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeRun(t *testing.T, dir string, models []string, local map[string]LoadedModel, attempts []Attempt) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, a := range attempts {
		if err := enc.Encode(a); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	meta, _ := json.Marshal(map[string]any{"models": models, "local_models": local, "reasoning_effort": "none"})
	if err := os.WriteFile(filepath.Join(dir, "run.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSiteDataCombinesRunsAndAliases(t *testing.T) {
	c := &Case{ID: "ls", Question: "list", Tags: []string{"easy"}, Reference: []string{"ls"}, Checks: []Check{{Program: "ls"}}}
	tmp := t.TempDir()
	const copyName, real = "ollama/hm-x:32k", "ollama/x:7b"
	writeRun(t, filepath.Join(tmp, "r1"), []string{copyName}, map[string]LoadedModel{copyName: {MemoryGB: 1, ResidentGB: 6}},
		[]Attempt{{Model: copyName, CaseID: "ls", Repeat: 1, Command: "ls", LatencyMS: 1000}})
	writeRun(t, filepath.Join(tmp, "r2"), []string{copyName}, map[string]LoadedModel{copyName: {MemoryGB: 1, ResidentGB: 7}},
		[]Attempt{{Model: copyName, CaseID: "ls", Repeat: 1, Command: "rm -rf /", LatencyMS: 3000}})

	data, err := BuildSiteData([]*Case{c}, []string{filepath.Join(tmp, "r1"), filepath.Join(tmp, "r2")}, "darwin", "today", map[string]string{copyName: real})
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Models) != 1 || data.Models[0].Model != real {
		t.Fatalf("models: %+v", data.Models)
	}
	m := data.Models[0]
	if m.N != 2 || m.Pass != 1 {
		t.Errorf("want 1/2 combined, got %d/%d", m.Pass, m.N)
	}
	cell := m.Results["ls"]
	if cell.Attempts != 2 || cell.Passes != 1 || cell.Command != "rm -rf /" {
		t.Errorf("cell: %+v", cell)
	}
	if m.MemoryGB == nil || *m.MemoryGB != 7 {
		t.Errorf("want peak resident 7 GB, got %v", m.MemoryGB)
	}
	if !m.Local || m.Reasoning != "none" {
		t.Errorf("local/reasoning: %+v", m)
	}
}
