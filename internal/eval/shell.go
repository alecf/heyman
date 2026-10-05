package eval

import (
	"regexp"
	"slices"
	"strings"
)

// This file implements a small, deliberately forgiving shell "parser" that
// answers one question: which programs does a command line invoke? It is not
// a full shell grammar. It understands enough to handle what models emit:
// pipelines and lists (| || && ; & newline), subshells and groups, quoting,
// $( ) / backtick / <( ) substitutions, redirections (including heredocs),
// leading VAR=value assignments, wrapper commands (sudo, env, xargs, nohup,
// time, timeout, nice, command, exec, …), `sh -c '…'`, and find -exec. Model
// placeholders such as <file> or <PID> are treated as plain words.

const maxDepth = 8

// Programs returns the distinct programs (basenames) invoked anywhere in cmd,
// in order of first appearance.
func Programs(cmd string) []string {
	var out []string
	collectPrograms(cmd, 0, &out)
	return out
}

// InvokesProgram reports whether cmd invokes program anywhere.
func InvokesProgram(cmd, program string) bool {
	return slices.Contains(Programs(cmd), program)
}

func collectPrograms(cmd string, depth int, out *[]string) {
	if depth > maxDepth {
		return
	}
	segs, subs := lex(cmd)
	for _, seg := range segs {
		segmentPrograms(seg, depth, out)
	}
	for _, sub := range subs {
		collectPrograms(sub, depth+1, out)
	}
}

func addProgram(out *[]string, name string) {
	if !slices.Contains(*out, name) {
		*out = append(*out, name)
	}
}

var (
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)
	// placeholder matches model placeholders like <file>, <PID>, <new name>.
	placeholder = regexp.MustCompile(`^<[A-Za-z][A-Za-z0-9_.:/-]*( [A-Za-z0-9_.:/-]+)*>`)
	// Placeholder reports placeholders anywhere in a command (for refusing exec).
	placeholderAnywhere = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9_.:/-]*( [A-Za-z0-9_.:/-]+)*>`)
)

// HasPlaceholder reports whether cmd contains a placeholder like <file>.
func HasPlaceholder(cmd string) bool { return placeholderAnywhere.MatchString(cmd) }

// segmentPrograms extracts programs from one simple command's words.
func segmentPrograms(words []string, depth int, out *[]string) {
	if depth > maxDepth {
		return
	}
	i := 0
	for i < len(words) {
		w := words[i]
		if assignment.MatchString(w) {
			i++
			continue
		}
		switch w {
		case "!", "{", "}", "then", "else", "elif", "do", "if", "while", "until", "fi", "done", "esac", "time", "-p":
			// "-p" only reaches here after the `time` keyword.
			i++
			continue
		case "for", "case", "select", "function", "[[", "]]":
			return
		}
		break
	}
	if i >= len(words) {
		return
	}
	w := words[i]
	if strings.HasPrefix(w, "$") || strings.HasPrefix(w, "-") || w == "" {
		return
	}
	name := w[strings.LastIndex(w, "/")+1:]
	if name == "" {
		return
	}
	addProgram(out, name)
	rest := words[i+1:]

	switch name {
	case "sudo", "doas":
		rest = skipOptions(rest, "uUgCDhprtT")
		segmentPrograms(rest, depth+1, out)
	case "env":
		rest = skipOptions(rest, "uSCP")
		for len(rest) > 0 && assignment.MatchString(rest[0]) {
			rest = rest[1:]
		}
		segmentPrograms(rest, depth+1, out)
	case "command", "builtin", "exec", "nohup", "caffeinate", "chronic":
		if name == "command" && len(rest) > 0 && (rest[0] == "-v" || rest[0] == "-V") {
			return // `command -v foo` looks foo up; it doesn't run it
		}
		rest = skipOptions(rest, "a")
		segmentPrograms(rest, depth+1, out)
	case "time", "gtime":
		rest = skipOptions(rest, "fo")
		segmentPrograms(rest, depth+1, out)
	case "nice":
		rest = skipOptions(rest, "n")
		segmentPrograms(rest, depth+1, out)
	case "stdbuf":
		rest = skipOptions(rest, "ioe")
		segmentPrograms(rest, depth+1, out)
	case "timeout", "gtimeout":
		rest = skipOptions(rest, "sk")
		if len(rest) > 0 {
			rest = rest[1:] // duration
		}
		segmentPrograms(rest, depth+1, out)
	case "xargs", "gxargs":
		rest = skipXargsOptions(rest)
		segmentPrograms(rest, depth+1, out)
	case "watch":
		rest = skipOptions(rest, "n")
		if len(rest) > 0 {
			collectPrograms(strings.Join(rest, " "), depth+1, out)
		}
	case "sh", "bash", "zsh", "dash", "ksh":
		for j := 0; j < len(rest); j++ {
			a := rest[j]
			if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
				break
			}
			if strings.Contains(a, "c") && j+1 < len(rest) {
				collectPrograms(rest[j+1], depth+1, out)
				break
			}
		}
	case "find", "gfind":
		for j := 0; j < len(rest); j++ {
			switch rest[j] {
			case "-exec", "-execdir", "-ok", "-okdir":
				k := j + 1
				for k < len(rest) && rest[k] != ";" && !(rest[k] == "+" && k > j+1 && rest[k-1] == "{}") {
					k++
				}
				segmentPrograms(rest[j+1:k], depth+1, out)
				j = k
			}
		}
	}
}

// skipOptions drops leading option words. Single-letter options listed in
// withValue take the next word as their value unless it is attached
// ("-uroot"). "--" ends option parsing. Long options "--x=y" are single words.
func skipOptions(words []string, withValue string) []string {
	for len(words) > 0 {
		w := words[0]
		if w == "--" {
			return words[1:]
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			return words
		}
		words = words[1:]
		if strings.HasPrefix(w, "--") {
			continue
		}
		// -abc: if the last letter takes a value and nothing is attached
		// after it, the value is the next word.
		last := w[len(w)-1:]
		if strings.Contains(withValue, last) && len(words) > 0 {
			// Only when the value-taking letter is the last one in the cluster.
			if !isNumeric(w[1:]) {
				words = words[1:]
			}
		}
	}
	return words
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// xargs options that take a value (BSD and GNU).
const xargsValueOpts = "IJLnPsEdaRS"

var xargsLongWithValue = []string{"--max-args", "--max-procs", "--max-lines", "--delimiter", "--arg-file", "--eof", "--replace", "--max-chars", "--process-slot-var"}

func skipXargsOptions(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		if w == "--" {
			return words[1:]
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			return words
		}
		words = words[1:]
		if strings.HasPrefix(w, "--") {
			if !strings.Contains(w, "=") && slices.Contains(xargsLongWithValue, w) && len(words) > 0 {
				words = words[1:]
			}
			continue
		}
		// Walk the cluster: the first value-taking letter consumes the rest
		// of the word, or the next word if nothing is attached.
		for k := 1; k < len(w); k++ {
			if strings.IndexByte(xargsValueOpts, w[k]) >= 0 {
				if k == len(w)-1 && len(words) > 0 {
					words = words[1:]
				}
				break
			}
		}
	}
	return words
}

// lex splits cmd into simple commands (each a list of unquoted words) and
// returns the bodies of command substitutions found anywhere, including
// inside double quotes.
func lex(cmd string) (segs [][]string, subs []string) {
	l := &lexer{s: cmd}
	l.run()
	l.endSegment()
	return l.segs, l.subs
}

type lexer struct {
	s    string
	i    int
	segs [][]string
	subs []string

	seg      []string
	word     strings.Builder
	inWord   bool
	dropNext bool // next completed word is a redirection target
	heredocs []heredoc
}

type heredoc struct {
	delim string
	strip bool // <<-
}

func (l *lexer) flush() {
	if !l.inWord {
		return
	}
	w := l.word.String()
	l.word.Reset()
	l.inWord = false
	if l.dropNext {
		l.dropNext = false
		return
	}
	l.seg = append(l.seg, w)
}

func (l *lexer) endSegment() {
	l.flush()
	l.dropNext = false
	if len(l.seg) > 0 {
		l.segs = append(l.segs, l.seg)
	}
	l.seg = nil
}

func (l *lexer) peek(off int) byte {
	if l.i+off < len(l.s) {
		return l.s[l.i+off]
	}
	return 0
}

func (l *lexer) run() {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.flush()
			l.i++
		case c == '\n':
			l.endSegment()
			l.i++
			l.skipHeredocBodies()
		case c == '\\':
			if l.peek(1) == '\n' {
				l.i += 2 // line continuation
				continue
			}
			if l.i+1 < len(l.s) {
				l.word.WriteByte(l.s[l.i+1])
			}
			l.inWord = true
			l.i += 2
		case c == '\'':
			end := strings.IndexByte(l.s[l.i+1:], '\'')
			if end < 0 {
				l.word.WriteString(l.s[l.i+1:])
				l.i = len(l.s)
			} else {
				l.word.WriteString(l.s[l.i+1 : l.i+1+end])
				l.i += end + 2
			}
			l.inWord = true
		case c == '"':
			l.doubleQuoted()
		case c == '$' && l.peek(1) == '(':
			l.dollarParen()
		case c == '$' && l.peek(1) == '\'':
			// ANSI-C quoting $'…'
			l.i++
			end := l.findANSIEnd()
			l.word.WriteString(l.s[min(l.i+1, end):end])
			l.i = end + 1
			l.inWord = true
		case c == '`':
			l.backtick()
		case c == '#' && !l.inWord:
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case (c == '<' || c == '>') && l.peek(1) == '(':
			// process substitution
			l.flush()
			l.i++
			body, end := matchParen(l.s, l.i)
			l.subs = append(l.subs, body)
			l.i = end
		case c == '<' && placeholder.MatchString(l.s[l.i:]):
			m := placeholder.FindString(l.s[l.i:])
			l.word.WriteString(m)
			l.inWord = true
			l.i += len(m)
		case c == '<' || c == '>':
			l.redirect()
		case c == '|':
			l.endSegment()
			l.i++
			if l.peek(0) == '|' || l.peek(0) == '&' {
				l.i++
			}
		case c == '&':
			if l.peek(1) == '>' {
				l.flush()
				l.i++
				l.redirect()
				continue
			}
			l.endSegment()
			l.i++
			if l.peek(0) == '&' {
				l.i++
			}
		case c == ';' || c == '(' || c == ')':
			l.endSegment()
			l.i++
		default:
			l.word.WriteByte(c)
			l.inWord = true
			l.i++
		}
	}
}

func (l *lexer) findANSIEnd() int {
	// l.i points at the opening quote.
	for j := l.i + 1; j < len(l.s); j++ {
		if l.s[j] == '\\' {
			j++
			continue
		}
		if l.s[j] == '\'' {
			return j
		}
	}
	return len(l.s)
}

func (l *lexer) doubleQuoted() {
	l.i++ // opening quote
	l.inWord = true
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == '"':
			l.i++
			return
		case c == '\\' && l.i+1 < len(l.s):
			l.word.WriteByte(l.s[l.i+1])
			l.i += 2
		case c == '$' && l.peek(1) == '(':
			l.dollarParen()
			l.inWord = true
		case c == '`':
			l.backtick()
			l.inWord = true
		default:
			l.word.WriteByte(c)
			l.i++
		}
	}
}

// dollarParen handles $( … ) and $(( … )) starting at l.i.
func (l *lexer) dollarParen() {
	start := l.i
	arith := l.peek(2) == '('
	body, end := matchParen(l.s, l.i+1)
	if !arith {
		l.subs = append(l.subs, body)
	}
	l.word.WriteString(l.s[start:min(end, len(l.s))])
	l.inWord = true
	l.i = end
}

func (l *lexer) backtick() {
	start := l.i
	j := l.i + 1
	for j < len(l.s) && l.s[j] != '`' {
		if l.s[j] == '\\' {
			j++
		}
		j++
	}
	body := l.s[l.i+1 : min(j, len(l.s))]
	l.subs = append(l.subs, body)
	l.i = min(j+1, len(l.s))
	l.word.WriteString(l.s[start:l.i])
	l.inWord = true
}

// matchParen returns the body between the '(' at s[open] and its matching
// ')', and the index just past the ')'. Quotes and nesting are respected.
func matchParen(s string, open int) (string, int) {
	depth := 0
	for j := open; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '\'':
			if k := strings.IndexByte(s[j+1:], '\''); k >= 0 {
				j += k + 1
			} else {
				j = len(s)
			}
		case '"':
			for j++; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' {
					j++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : j], j + 1
			}
		}
	}
	return s[min(open+1, len(s)):], len(s)
}

// redirect consumes a redirection operator at l.i and arranges for its target
// word to be dropped. A pending all-digit word (the fd, as in 2>) is dropped.
func (l *lexer) redirect() {
	if l.inWord && isNumeric(l.word.String()) {
		l.word.Reset()
		l.inWord = false
	} else {
		l.flush()
	}
	c := l.s[l.i]
	l.i++
	if c == '<' && l.peek(0) == '<' {
		l.i++
		if l.peek(0) == '<' { // here-string <<<
			l.i++
			l.dropNext = true
			return
		}
		strip := false
		if l.peek(0) == '-' {
			strip = true
			l.i++
		}
		for l.peek(0) == ' ' || l.peek(0) == '\t' {
			l.i++
		}
		// Read the delimiter word (possibly quoted).
		var d strings.Builder
		for l.i < len(l.s) {
			ch := l.s[l.i]
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == ';' || ch == '|' || ch == '&' || ch == '<' || ch == '>' || ch == ')' {
				break
			}
			if ch != '\'' && ch != '"' && ch != '\\' {
				d.WriteByte(ch)
			}
			l.i++
		}
		if d.Len() > 0 {
			l.heredocs = append(l.heredocs, heredoc{delim: d.String(), strip: strip})
		}
		return
	}
	switch l.peek(0) {
	case '>', '|':
		l.i++
	case '&':
		l.i++
		// >&2, <&0, >&- : fd duplication, no word target.
		j := l.i
		for j < len(l.s) && (l.s[j] >= '0' && l.s[j] <= '9' || l.s[j] == '-') {
			j++
		}
		if j > l.i {
			l.i = j
			return
		}
	}
	if c == '<' && l.peek(0) == '>' {
		l.i++
	}
	l.dropNext = true
}

func (l *lexer) skipHeredocBodies() {
	for len(l.heredocs) > 0 {
		h := l.heredocs[0]
		l.heredocs = l.heredocs[1:]
		for l.i < len(l.s) {
			end := strings.IndexByte(l.s[l.i:], '\n')
			var line string
			if end < 0 {
				line = l.s[l.i:]
				l.i = len(l.s)
			} else {
				line = l.s[l.i : l.i+end]
				l.i += end + 1
			}
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				break
			}
		}
	}
}
