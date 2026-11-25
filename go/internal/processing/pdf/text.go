package pdf

// The text of a page. ledongthuc/pdf parses the content and decodes the fonts, but the walk over the operators is
// here: its Page.Content ends every TJ with a "\n" that goes through the current font (so TeX's math fonts leave an
// Ω or a ⊗ there), knows no word spaces (TeX moves the pen instead of writing them) and misreads TeX's bitmap
// Cyrillic fonts. The walk follows Page.Content, Copyright (c) 2009 The Go Authors, BSD-3-Clause.

import (
	"math"
	"strconv"
	"strings"

	textpdf "github.com/ledongthuc/pdf"
)

const (
	spaceGap = 0.18 // of the font's size: a wider gap between two glyphs is a space
	lineGap  = 0.5  // of the font's size: a baseline that moves more starts a line (sub- and superscripts stay)
	kernGap  = -200 // thousandths in a TJ: a space, where the glyphs' widths are unknown
)

// matrix is a PDF transformation [a b 0; c d 0; e f 1].
type matrix struct{ a, b, c, d, e, f float64 }

var identity = matrix{a: 1, d: 1}

func translate(x, y float64) matrix { return matrix{a: 1, d: 1, e: x, f: y} }

// then is m followed by n.
func (m matrix) then(n matrix) matrix {
	return matrix{m.a*n.a + m.b*n.c, m.a*n.b + m.b*n.d, m.c*n.a + m.d*n.c, m.c*n.b + m.d*n.d,
		m.e*n.a + m.f*n.c + n.e, m.e*n.b + m.f*n.d + n.f}
}

type glyph struct {
	text  string
	width float64 // in text space at size 1; 0 when the font does not say
}

type font struct {
	f         textpdf.Font
	enc       textpdf.TextEncoding
	composite bool    // a Type0 font: its glyphs are not single bytes
	identity  bool    // a composite whose every two bytes are a glyph's number
	cyrillic  bool    // a TeX bitmap font in T2A
	unit      float64 // what one unit of Widths is in text space
	widths    cidWidths
	glyphs    map[int]glyph
}

func newFont(f textpdf.Font) *font {
	out := &font{f: f, enc: f.Encoder(), unit: 0.001, glyphs: map[int]glyph{}}
	switch f.V.Key("Subtype").Name() {
	case "Type0":
		out.composite = true
		switch f.V.Key("Encoding").Name() {
		case "Identity-H", "Identity-V":
			out.identity = true
			out.widths = readWidths(f.V.Key("DescendantFonts").Index(0))
		}
	case "Type3":
		out.unit = f.V.Key("FontMatrix").Index(0).Float64()
		out.cyrillic = bitmapCyrillic(f.V.Key("Encoding").Key("Differences"))
	}
	return out
}

// glyph is the glyph of a code: a byte, or the two bytes of an identity composite.
func (f *font) glyph(raw string) glyph {
	code := 0
	for i := range len(raw) {
		code = code<<8 | int(raw[i])
	}
	g, ok := f.glyphs[code]
	if !ok {
		if f.cyrillic {
			g.text = t2a(raw[0])
		} else {
			g.text = f.enc.Decode(raw)
		}
		if f.identity {
			g.width = f.widths.of(code) * f.unit
		} else {
			g.width = f.f.Width(code) * f.unit
		}
		f.glyphs[code] = g
	}
	return g
}

// cidWidths are a composite font's widths: its W array, whose entries are "first [w w …]" or "first last w", over
// the default DW.
type cidWidths struct {
	def    float64
	listed map[int]float64
	ranges []struct {
		first, last int
		width       float64
	}
}

func readWidths(descendant textpdf.Value) cidWidths {
	out := cidWidths{def: 1000, listed: map[int]float64{}}
	if dw := descendant.Key("DW"); dw.Kind() != textpdf.Null {
		out.def = dw.Float64()
	}
	w := descendant.Key("W")
	for i, n := 0, w.Len(); i+1 < n; {
		first := int(w.Index(i).Int64())
		if next := w.Index(i + 1); next.Kind() == textpdf.Array {
			for j := range next.Len() {
				out.listed[first+j] = next.Index(j).Float64()
			}
			i += 2
		} else {
			out.ranges = append(out.ranges, struct {
				first, last int
				width       float64
			}{first, int(next.Int64()), w.Index(i + 2).Float64()})
			i += 3
		}
	}
	return out
}

func (w cidWidths) of(code int) float64 {
	if width, ok := w.listed[code]; ok {
		return width
	}
	for _, r := range w.ranges {
		if code >= r.first && code <= r.last {
			return r.width
		}
	}
	return w.def
}

// bitmapCyrillic tells a Cyrillic font among pdfTeX's bitmap ones. Their glyph names are only the codes ("a224" at
// 224) and they carry no map to Unicode, so the encoding is guessed by what the font holds: a Cyrillic font (T2A)
// has more letters from 192 on than ASCII ones, a Latin one (T1, with its accented letters there) has not.
func bitmapCyrillic(differences textpdf.Value) bool {
	code, high, ascii := 0, 0, 0
	for i := range differences.Len() {
		switch x := differences.Index(i); x.Kind() {
		case textpdf.Integer:
			code = int(x.Int64())
		case textpdf.Name:
			if name := x.Name(); name != ".notdef" {
				if name != "a"+strconv.Itoa(code) {
					return false
				}
				switch {
				case code >= 192:
					high++
				case code >= 'A' && code <= 'Z', code >= 'a' && code <= 'z':
					ascii++
				}
			}
			code++
		}
	}
	return high > ascii
}

// t2aOther are T2A's codes outside ASCII and the alphabet at 192–255 (t2aenc.def; ligatures as in T1).
var t2aOther = map[byte]string{13: "Ӏ", 14: "⟨", 15: "⟩", 16: "“", 17: "”", 21: "–", 22: "—", 25: "ı", 26: "ȷ",
	27: "ff", 28: "fi", 29: "fl", 30: "ffi", 31: "ffl", 39: "’", 96: "‘", 127: "-", 128: "Ґ", 136: "Ї", 146: "Ў",
	153: "Є", 156: "Ё", 157: "№", 158: "¤", 159: "§", 160: "ґ", 168: "ї", 178: "ў", 185: "є", 188: "ё", 189: "„",
	190: "«", 191: "»"}

func t2a(code byte) string {
	if code >= 192 {
		return string(rune('А' + int(code) - 192))
	}
	if s, ok := t2aOther[code]; ok {
		return s
	}
	if code >= 32 && code < 127 {
		return string(rune(code))
	}
	return "" // accents and letters of other languages
}

type state struct {
	ctm, tm, line                matrix
	font                         *font
	size, tc, tw, th, lead, rise float64
}

// writer joins the glyphs into lines and words by where they stand.
type writer struct {
	out            strings.Builder
	started        bool
	y, end, em     float64
	endKnown, kern bool
}

func (w *writer) put(text string, x, y, end, em float64, endKnown bool) {
	if text == "" {
		return
	}
	if w.started {
		size := max(em, w.em)
		switch {
		case math.Abs(y-w.y) > max(1, lineGap*size):
			w.out.WriteByte('\n')
		case w.endKnown && x-w.end > spaceGap*size, !w.endKnown && w.kern:
			w.out.WriteByte(' ')
		}
	}
	w.out.WriteString(text)
	w.started, w.y, w.end, w.em, w.endKnown, w.kern = true, y, end, em, endKnown, false
}

// show writes a string of the current font and moves the pen past it.
func (s *state) show(w *writer, raw string) {
	if s.font == nil {
		return
	}
	one := func(g glyph, space bool) {
		m := s.tm.then(s.ctm)
		at := matrix{a: s.size * s.th, d: s.size, f: s.rise}.then(m)
		w.put(g.text, at.e, at.f, at.e+g.width*s.size*s.th*m.a, math.Hypot(at.c, at.d), g.width > 0)
		move := g.width*s.size + s.tc
		if space {
			move += s.tw
		}
		s.tm = translate(move*s.th, 0).then(s.tm)
	}
	switch {
	case s.font.identity:
		for i := 0; i+1 < len(raw); i += 2 {
			one(s.font.glyph(raw[i:i+2]), false)
		}
	case s.font.composite: // another encoding: where its glyphs end is not known
		one(glyph{text: s.font.enc.Decode(raw)}, false)
	default:
		for i := range len(raw) {
			one(s.font.glyph(raw[i:i+1]), raw[i] == ' ')
		}
	}
}

// readPage is the page's text in the order it is drawn. It panics on a broken page.
func readPage(page textpdf.Page) string {
	if page.V.IsNull() || page.V.Key("Contents").Kind() == textpdf.Null {
		return ""
	}
	var (
		w      writer
		stack  []state
		fonts  = map[string]*font{}
		s      = state{ctm: identity, tm: identity, line: identity, th: 1}
		newRow = func(x, y float64) {
			s.line = translate(x, y).then(s.line)
			s.tm = s.line
		}
	)
	textpdf.Interpret(page.V.Key("Contents"), func(stk *textpdf.Stack, op string) {
		args := make([]textpdf.Value, stk.Len())
		for i := len(args) - 1; i >= 0; i-- {
			args[i] = stk.Pop()
		}
		num := func(i int) float64 { return args[i].Float64() }
		six := func() matrix { return matrix{num(0), num(1), num(2), num(3), num(4), num(5)} }
		switch op { // an operator short of its operands panics, and the page gives no text
		case "q":
			stack = append(stack, s)
		case "Q":
			if n := len(stack) - 1; n >= 0 {
				s, stack = stack[n], stack[:n]
			}
		case "cm":
			s.ctm = six().then(s.ctm)
		case "BT":
			s.tm, s.line = identity, identity
		case "Tm":
			s.line = six()
			s.tm = s.line
		case "Td":
			newRow(num(0), num(1))
		case "TD":
			s.lead = -num(1)
			newRow(num(0), num(1))
		case "T*":
			newRow(0, -s.lead)
		case "TL":
			s.lead = num(0)
		case "Tc":
			s.tc = num(0)
		case "Tw":
			s.tw = num(0)
		case "Tz":
			s.th = num(0) / 100
		case "Ts":
			s.rise = num(0)
		case "Tf":
			name := args[0].Name()
			if fonts[name] == nil {
				fonts[name] = newFont(page.Font(name))
			}
			s.font, s.size = fonts[name], num(1)
		case "Tj":
			s.show(&w, args[0].RawString())
		case "'":
			newRow(0, -s.lead)
			s.show(&w, args[0].RawString())
		case "\"":
			s.tw, s.tc = num(0), num(1)
			newRow(0, -s.lead)
			s.show(&w, args[2].RawString())
		case "TJ":
			for i := range args[0].Len() {
				if x := args[0].Index(i); x.Kind() == textpdf.String {
					s.show(&w, x.RawString())
				} else {
					w.kern = w.kern || x.Float64() <= kernGap
					s.tm = translate(-x.Float64()/1000*s.size*s.th, 0).then(s.tm)
				}
			}
		}
	})
	return w.out.String()
}
