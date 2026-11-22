package pdf

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// --- difflib ------------------------------------------------------------------------------------------------------

// matcher is Python's difflib.SequenceMatcher without isjunk, with autojunk: the blocks and opcodes come out as
// Python's, so the changed lines and the name ratio are the same.
type matcher[T comparable] struct {
	a, b []T
	b2j  map[T][]int
}

func newMatcher[T comparable](a, b []T) *matcher[T] {
	m := &matcher[T]{a: a, b: b, b2j: map[T][]int{}}
	for j, x := range b {
		m.b2j[x] = append(m.b2j[x], j)
	}
	if n := len(b); n >= 200 { // popular elements are no anchors
		ntest := n/100 + 1
		for x, idx := range m.b2j {
			if len(idx) > ntest {
				delete(m.b2j, x)
			}
		}
	}
	return m
}

func (m *matcher[T]) longest(alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		next := map[int]int{}
		for _, j := range m.b2j[m.a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			next[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = next
	}
	for besti > alo && bestj > blo && m.a[besti-1] == m.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && m.a[besti+bestsize] == m.b[bestj+bestsize] {
		bestsize++
	}
	return besti, bestj, bestsize
}

func (m *matcher[T]) blocks() [][3]int {
	queue := [][4]int{{0, len(m.a), 0, len(m.b)}}
	var found [][3]int
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, k := m.longest(q[0], q[1], q[2], q[3])
		if k > 0 {
			found = append(found, [3]int{i, j, k})
			if q[0] < i && q[2] < j {
				queue = append(queue, [4]int{q[0], i, q[2], j})
			}
			if i+k < q[1] && j+k < q[3] {
				queue = append(queue, [4]int{i + k, q[1], j + k, q[3]})
			}
		}
	}
	for x := 1; x < len(found); x++ { // sort
		for y := x; y > 0 && less(found[y], found[y-1]); y-- {
			found[y], found[y-1] = found[y-1], found[y]
		}
	}
	var out [][3]int
	i1, j1, k1 := 0, 0, 0
	for _, blk := range found {
		if i1+k1 == blk[0] && j1+k1 == blk[1] {
			k1 += blk[2]
			continue
		}
		if k1 > 0 {
			out = append(out, [3]int{i1, j1, k1})
		}
		i1, j1, k1 = blk[0], blk[1], blk[2]
	}
	if k1 > 0 {
		out = append(out, [3]int{i1, j1, k1})
	}
	return append(out, [3]int{len(m.a), len(m.b), 0})
}

func less(x, y [3]int) bool {
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

type opcode struct {
	tag            string
	i1, i2, j1, j2 int
}

func (m *matcher[T]) opcodes() []opcode {
	var out []opcode
	i, j := 0, 0
	for _, blk := range m.blocks() {
		tag := ""
		switch {
		case i < blk[0] && j < blk[1]:
			tag = "replace"
		case i < blk[0]:
			tag = "delete"
		case j < blk[1]:
			tag = "insert"
		}
		if tag != "" {
			out = append(out, opcode{tag, i, blk[0], j, blk[1]})
		}
		i, j = blk[0]+blk[2], blk[1]+blk[2]
		if blk[2] > 0 {
			out = append(out, opcode{"equal", blk[0], i, blk[1], j})
		}
	}
	return out
}

func (m *matcher[T]) ratio() float64 {
	total := len(m.a) + len(m.b)
	if total == 0 {
		return 1
	}
	matches := 0
	for _, blk := range m.blocks() {
		matches += blk[2]
	}
	return 2 * float64(matches) / float64(total)
}

// Diff is the changed lines of two texts, "-" old and "+" new, at most limit characters; "" when the text is the
// same. Only this goes to the model when it describes a new version (D14).
func Diff(old, new string, limit int) string {
	a, b := splitLines(old), splitLines(new)
	var lines []string
	add := func(prefix string, part []string) {
		for _, line := range part {
			line = prefix + line
			if !strings.HasPrefix(line, "+++") && !strings.HasPrefix(line, "---") {
				lines = append(lines, line)
			}
		}
	}
	for _, op := range newMatcher(a, b).opcodes() {
		if op.tag == "replace" || op.tag == "delete" {
			add("-", a[op.i1:op.i2])
		}
		if op.tag == "replace" || op.tag == "insert" {
			add("+", b[op.j1:op.j2])
		}
	}
	text := strings.Join(lines, "\n")
	if pytext.Len(text) <= limit {
		return text
	}
	cut := pytext.Cut(text, limit)
	if i := strings.LastIndex(cut, "\n"); i >= 0 {
		cut = cut[:i]
	}
	return cut + "\n…"
}

// --- names of files -------------------------------------------------------------------------------------------------

const NameSimilarity = 0.8

var (
	extension  = regexp.MustCompile(`\.[A-Za-z0-9]{1,5}$`)
	separators = regexp.MustCompile(`[_\-.,\[\]{}]+`)
	parens     = regexp.MustCompile(`[()]+`)
	numbersRe  = regexp.MustCompile(`\p{Nd}+`)
)

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func isCyrillicLower(r rune) bool {
	return r >= 'а' && r <= 'я' || r == 'ё' || r >= 'А' && r <= 'Я' || r == 'Ё'
}

func hasPrefix(s []rune, i int, word string) bool {
	w := []rune(word)
	return i+len(w) <= len(s) && string(s[i:i+len(w)]) == word
}

func boundary(s []rune, i int) bool { // \b before or after a word character at i
	before := i > 0 && isWord(s[i-1])
	after := i < len(s) && isWord(s[i])
	return before != after
}

func digitsEnd(s []rune, i int) int {
	for i < len(s) && unicode.IsDigit(s[i]) {
		i++
	}
	return i
}

func wordEnd(s []rune, i int) int {
	for i < len(s) && isWord(s[i]) {
		i++
	}
	return i
}

// versionAt matches VERSION_WORDS at s[i] (the text is in lower case): `\((?:\d+)\)`, `\bv(?:er)?\.?\s*\d+\b`,
// `\b(?:new|final|fixed|upd(?:ated)?|copy|rev\d*)\b`, `(?<![а-яё])(?:нов\w*|испр\w*|обновл\w*|финал\w*|копия|верси\w*)`.
func versionAt(s []rune, i int) int {
	if s[i] == '(' {
		if j := digitsEnd(s, i+1); j > i+1 && j < len(s) && s[j] == ')' {
			return j + 1
		}
	}
	if s[i] == 'v' && boundary(s, i) {
		for _, start := range []int{i + 3, i + 1} {
			if start == i+3 && !hasPrefix(s, i, "ver") {
				continue
			}
			j := start
			if j < len(s) && s[j] == '.' {
				j++
			}
			for j < len(s) && pytext.IsSpace(s[j]) {
				j++
			}
			if k := digitsEnd(s, j); k > j && boundary(s, k) {
				return k
			}
		}
	}
	if boundary(s, i) {
		for _, word := range []string{"new", "final", "fixed", "updated", "upd", "copy"} {
			if hasPrefix(s, i, word) && boundary(s, i+len([]rune(word))) {
				return i + len([]rune(word))
			}
		}
		if hasPrefix(s, i, "rev") {
			if k := digitsEnd(s, i+3); boundary(s, k) {
				return k
			}
		}
	}
	if i == 0 || !isCyrillicLower(s[i-1]) {
		for _, stem := range []string{"нов", "испр", "обновл", "финал", "копия", "верси"} {
			if hasPrefix(s, i, stem) {
				end := i + len([]rune(stem))
				if stem != "копия" {
					end = wordEnd(s, end)
				}
				return end
			}
		}
	}
	return -1
}

// NameKey is a file name without its extension, case, separators and version marks.
func NameKey(name string) string {
	stem := extension.ReplaceAllString(pytext.Strip(name), "")
	stem = strings.ReplaceAll(strings.ToLower(stem), "ё", "е")
	s := []rune(separators.ReplaceAllString(stem, " ")) // "(1)" is a version mark: kept for it
	var out []rune
	for i := 0; i < len(s); {
		if end := versionAt(s, i); end > i {
			out = append(out, ' ')
			i = end
			continue
		}
		out = append(out, s[i])
		i++
	}
	return strings.Join(pytext.Split(parens.ReplaceAllString(string(out), " ")), " ")
}

// SimilarNames reports almost the same name (D14): the same numbers in it and most of the letters. "ДЗ3.pdf" and
// "ДЗ3_исправленный.pdf" are, "ДЗ3.pdf" and "ДЗ4.pdf" are not.
func SimilarNames(a, b string) bool {
	ka, kb := NameKey(a), NameKey(b)
	if ka == "" || kb == "" || strings.Join(numbersRe.FindAllString(ka, -1), " ") != strings.Join(numbersRe.FindAllString(kb, -1), " ") {
		return false
	}
	return ka == kb || newMatcher([]rune(ka), []rune(kb)).ratio() >= NameSimilarity
}
