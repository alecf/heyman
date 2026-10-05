package eval

import (
	"slices"
	"testing"
)

func TestPrograms(t *testing.T) {
	tests := []struct {
		cmd  string
		want []string
	}{
		{"ls -la", []string{"ls"}},
		{"lsof -ti tcp:3000 | xargs kill", []string{"lsof", "xargs", "kill"}},
		{"kill $(lsof -t -i:3000)", []string{"kill", "lsof"}},
		{"kill `lsof -t -i:3000`", []string{"kill", "lsof"}},
		{"a && b || c; d & e", []string{"a", "b", "c", "d", "e"}},
		{"FOO=1 BAR=2 make test", []string{"make"}},
		{"sudo -u root lsof -i :80", []string{"sudo", "lsof"}},
		{"env -i PATH=/bin LC_ALL=C sort file", []string{"env", "sort"}},
		{"nohup ./server.sh > log 2>&1 &", []string{"nohup", "server.sh"}},
		{"time find . -name x", []string{"find"}},
		{"/usr/bin/time -p du -sh .", []string{"time", "du"}},
		{"command grep foo", []string{"command", "grep"}},
		{"command -v jq", []string{"command"}},
		{"exec cat file", []string{"exec", "cat"}},
		{"nice -n 10 gzip big", []string{"nice", "gzip"}},
		{"timeout 5 curl -s x", []string{"timeout", "curl"}},
		{"find . -name '*.log' -print0 | xargs -0 -n 1 -P 4 gzip", []string{"find", "xargs", "gzip"}},
		{"xargs -I {} cp {} /tmp", []string{"xargs", "cp"}},
		{"xargs -I{} cp {} /tmp", []string{"xargs", "cp"}},
		{"xargs -n1 -P4 gzip", []string{"xargs", "gzip"}},
		{"xargs -L 1 -J % mv % dir", []string{"xargs", "mv"}},
		{"xargs --max-procs 4 --null gzip", []string{"xargs", "gzip"}},
		{"xargs -0 sudo rm", []string{"xargs", "sudo", "rm"}},
		{"find . -type f -exec stat -f '%z %N' {} +", []string{"find", "stat"}},
		{"find . -type f -exec wc -c {} \\;", []string{"find", "wc"}},
		{"find . -exec sh -c 'gzip \"$1\" && echo done' _ {} \\;", []string{"find", "sh", "gzip", "echo"}},
		{"bash -c 'ps aux | grep node'", []string{"bash", "ps", "grep"}},
		{"sh -ec 'a; b'", []string{"sh", "a", "b"}},
		// quoted separators are not split
		{"awk '{print $1}' | sort -u", []string{"awk", "sort"}},
		{"grep 'a|b; c' file", []string{"grep"}},
		{`echo "x | y"`, []string{"echo"}},
		// substitutions inside double quotes are still parsed
		{`echo "today is $(date +%F)"`, []string{"echo", "date"}},
		{"diff <(sort a) <(sort b)", []string{"diff", "sort"}},
		{"echo $((1+2))", []string{"echo"}},
		// redirections
		{"wc -l < filelist.txt", []string{"wc"}},
		{"sort file > out.txt 2>/dev/null", []string{"sort"}},
		{"cmd 2>&1 | tee log", []string{"cmd", "tee"}},
		{"cat <<EOF | sort\nb\na\nEOF\necho after", []string{"cat", "sort", "echo"}},
		{"tr a b <<< 'hello'", []string{"tr"}},
		{"cmd &> /dev/null", []string{"cmd"}},
		// placeholders
		{"kill -9 <PID>", []string{"kill"}},
		{"tar -xzf <archive.tar.gz> -C <dest dir>", []string{"tar"}},
		{"cat <file> | grep <pattern>", []string{"cat", "grep"}},
		{"curl --output=<file> https://x", []string{"curl"}},
		// compound commands
		{"for f in *.txt; do wc -l \"$f\"; done", []string{"wc"}},
		{"while IFS= read -r f; do cat \"$f\"; done < filelist.txt | wc -l", []string{"read", "cat", "wc"}},
		{"if grep -q x f; then echo yes; else echo no; fi", []string{"grep", "echo"}},
		{"(cd dir && make)", []string{"cd", "make"}},
		{"{ echo a; echo b; } | sort", []string{"echo", "sort"}},
		{"! grep -q x f", []string{"grep"}},
		// line continuation and comments
		{"find . \\\n  -name x | \\\n  wc -l", []string{"find", "wc"}},
		{"ls # list files | not-a-program", []string{"ls"}},
		{"echo $'a\\'b' | cat", []string{"echo", "cat"}},
		{"watch -n 1 'lsof -i :80'", []string{"watch", "lsof"}},
		{"", nil},
		{"   ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got := Programs(tt.cmd)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Programs(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestProgramsNoPanicOnMalformed(t *testing.T) {
	for _, s := range []string{
		"echo 'unterminated", `echo "unterminated`, "echo $(unterminated", "echo `unterminated",
		"a |", "| b", "a >", "<", "cat <<", "x\\", "$'", "<(", "find . -exec", "xargs -I", "sudo -u",
		"bash -c", "$(", ")))", "(((",
	} {
		_ = Programs(s)
	}
}

func TestHasPlaceholder(t *testing.T) {
	tests := map[string]bool{
		"kill <PID>":                      true,
		"cp <source file> <dest>":         true,
		"wc -l < filelist.txt":            false,
		"sort <in.txt >out.txt":           false,
		"diff <(sort a) <(sort b)":        false,
		"cat <<EOF":                       false,
		"echo 'a' 2>&1":                   false,
		"git log --format='<%an> %s'":     false,
		"grep -E '<[a-z]+>' file.html":    false,
		"ssh -L 8080:localhost:80 <host>": true,
	}
	for cmd, want := range tests {
		if got := HasPlaceholder(cmd); got != want {
			t.Errorf("HasPlaceholder(%q) = %v, want %v", cmd, got, want)
		}
	}
}
