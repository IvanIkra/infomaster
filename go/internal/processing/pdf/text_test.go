package pdf

import (
	"fmt"
	"strings"
	"testing"
)

// pageOf is the text of a PDF of one page that draws `content` with `font` as /F1; `more` are the objects the font
// refers to, numbered from 5.
func pageOf(t *testing.T, font, content string, more ...string) string {
	t.Helper()
	objects := append([]string{"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", 5+len(more)), font,
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)}, more...)
	objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << "+
		"/F1 3 0 R >> >> /Contents 4 0 R >>")
	extract, err := ExtractText(write(t, "page.pdf", assemble(objects)), 1)
	if err != nil {
		t.Fatal(err)
	}
	return extract.First()
}

func widths(first, last, width int) string {
	return fmt.Sprintf("/FirstChar %d /LastChar %d /Widths [%s]", first, last,
		strings.Repeat(fmt.Sprintf("%d ", width), last-first+1))
}

func cmapStream(body string) string {
	cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap\n" + body +
		"\nendcmap CMapName currentdict /CMap defineresource pop end end"
	return fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap)
}

func expectText(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}

// pdfTeX's bitmap fonts name a glyph only by its code and say nothing of Unicode.
func TestTeXBitmapFonts(t *testing.T) {
	font := func(differences string) string {
		return "<< /Type /Font /Subtype /Type3 /FontMatrix [0.01 0 0 0.01 0 0] " + widths(0, 255, 50) +
			" /Encoding << /Type /Encoding /Differences [" + differences + "] >> >>"
	}
	cyrillic := font("22/a22 188/a188 190/a190/a191 199/a199 224/a224 228/a228 247/a247")
	// a word space is a move of the pen: inside a TJ, or a new Td on the same line
	expectText(t, pageOf(t, cyrillic, "BT /F1 10 Tf 50 700 Td [<C7E0E4E0F7E0> -333 <BEE4BF> -20 <BC16>] TJ "+
		"65 0 Td <E4E0> Tj 0 -12 Td <E4> Tj ET"), "Задача «д»ё— да\nд")
	// the same kind of font with a Latin encoding (T1): its letters stay as they are
	latin := font("97/a97/a98/a99 233/a233")
	expectText(t, pageOf(t, latin, "BT /F1 10 Tf 50 700 Td <616263E9> Tj ET"), "abcé")
}

// A TJ is no line of its own and adds no glyph (code 10 is Ω in TeX's fonts); a subscript stays on its line.
func TestMathLines(t *testing.T) {
	font := "<< /Type /Font /Subtype /Type1 /BaseFont /CMR10 " + widths(32, 126, 500) + " /ToUnicode 5 0 R >>"
	got := pageOf(t, font, "BT /F1 10 Tf 50 700 Td [(x) -333 (y)] TJ 13.33 0 Td [(z)] TJ "+
		"/F1 7 Tf 5 -1.5 Td [(n)] TJ /F1 10 Tf 3.5 1.5 Td [(.)] TJ ET",
		cmapStream("1 begincodespacerange <00> <FF> endcodespacerange\n1 beginbfchar <0A> <2126> endbfchar\n"+
			"1 beginbfrange <20> <7E> <0020> endbfrange"))
	expectText(t, got, "x yzn.")
}

// A composite font's widths come from its descendant: pieces that touch are one word, a gap is a space.
func TestCompositeFonts(t *testing.T) {
	font := "<< /Type /Font /Subtype /Type0 /BaseFont /Any /Encoding /Identity-H /DescendantFonts [5 0 R] " +
		"/ToUnicode 6 0 R >>"
	got := pageOf(t, font, "BT /F1 10 Tf 50 700 Td <00010002> Tj 12 0 Td <0003> Tj 20 0 Td <0004> Tj ET",
		"<< /Type /Font /Subtype /CIDFontType2 /DW 400 /W [1 [600 600] 3 4 600] >>",
		cmapStream("1 begincodespacerange <0000> <FFFF> endcodespacerange\n"+
			"1 beginbfrange <0001> <0004> <0430> endbfrange"))
	expectText(t, got, "абв г")
}
