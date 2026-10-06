package eval

import "testing"

func TestParseOllamaPS(t *testing.T) {
	out := `NAME                 ID              SIZE      PROCESSOR          CONTEXT    UNTIL
hm-qwen3-5-9b:32k    72c99fdb7033    6.7 GB    100% GPU           32768      4 minutes from now
qwen3:0.6b           7df6b6e09427    900 MB    48%/52% CPU/GPU    4096       Forever
broken line
`
	got := ParseOllamaPS(out)
	if m := got["hm-qwen3-5-9b:32k"]; m.MemoryGB != 6.7 || m.Context != 32768 {
		t.Errorf("9b: %+v", m)
	}
	if m := got["qwen3:0.6b"]; m.MemoryGB < 0.87 || m.MemoryGB > 0.88 || m.Context != 4096 {
		t.Errorf("0.6b: %+v", m)
	}
	if len(got) != 2 {
		t.Errorf("rows: %v", got)
	}
}
