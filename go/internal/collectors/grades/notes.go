package grades

import (
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

var noteRef = regexp.MustCompile(`^([A-Z]+)([0-9]+)$`)

// SplitLines is Python's str.splitlines.
func SplitLines(text string) []string {
	var out []string
	s := []rune(text)
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(s[start:i]))
			if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, string(s[start:]))
	}
	return out
}

// ownNotes is the comments on the own row's cells of each tab, read from the xlsx's comment parts: tab -> column ->
// text. Only the own row's are taken, and never the author (a teacher's name).
func (b *book) ownNotes(ownRows map[string]int) (map[string]map[string]string, error) {
	found := map[string]map[string]string{}
	data, err := b.read("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	sheets, err := b.rels("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return found, nil
		}
		if err != nil {
			return nil, err
		}
		t, ok := tok.(xml.StartElement)
		if !ok || t.Name.Space != mainNS || t.Name.Local != "sheet" {
			continue
		}
		name := attr(t, "", "name")
		row, ok := ownRows[name]
		part := sheets[attr(t, docNS, "id")].target
		if !ok || b.files[part] == nil {
			continue
		}
		rels, err := b.rels(part)
		if err != nil {
			return nil, err
		}
		for _, rel := range sortedRels(rels) {
			if !strings.HasSuffix(rel.kind, "/comments") || b.files[rel.target] == nil {
				continue
			}
			if err := b.readComments(rel.target, row, func(col, note string) {
				if found[name] == nil {
					found[name] = map[string]string{}
				}
				found[name][col] = pytext.Cut(note, NoteChars)
			}); err != nil {
				return nil, err
			}
		}
	}
}

func (b *book) readComments(part string, row int, take func(col, note string)) error {
	data, err := b.read(part)
	if err != nil {
		return err
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		t, ok := tok.(xml.StartElement)
		if !ok || t.Name.Space != mainNS || t.Name.Local != "comment" {
			continue
		}
		ref := attr(t, "", "ref")
		var text strings.Builder
		depth, inT := 1, 0
		for depth > 0 {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			switch e := tok.(type) {
			case xml.StartElement:
				depth++
				if e.Name.Space == mainNS && e.Name.Local == "t" {
					inT++
				}
			case xml.EndElement:
				depth--
				if e.Name.Space == mainNS && e.Name.Local == "t" {
					inT--
				}
			case xml.CharData:
				if inT > 0 {
					text.Write(e)
				}
			}
		}
		m := noteRef.FindStringSubmatch(strings.ReplaceAll(ref, "$", ""))
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[2]); err != nil || n != row {
			continue
		}
		lines := SplitLines(pytext.Strip(text.String()))
		for i, line := range lines {
			lines[i] = pytext.Strip(line)
		}
		if note := pytext.Strip(strings.Join(lines, "\n")); note != "" {
			take(m[1], note)
		}
	}
}
