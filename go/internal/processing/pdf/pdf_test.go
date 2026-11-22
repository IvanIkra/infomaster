package pdf

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makePDF is a PDF with one page per text (ASCII, one line each, Helvetica); "" makes a page without text, like a
// scan.
func makePDF(pages []string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var kids []string
	for _, text := range pages {
		var stream strings.Builder
		if text != "" {
			for i, line := range strings.Split(text, "\n") {
				line = strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(line)
				fmt.Fprintf(&stream, "BT /F1 12 Tf 50 %d Td (%s) Tj ET\n", 750-16*i, line)
			}
		}
		objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", stream.Len(), stream.String()))
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << "+
			"/Font << /F1 3 0 R >> >> /Contents %d 0 R >>", len(objects)))
		kids = append(kids, fmt.Sprintf("%d 0 R", len(objects)))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	return assemble(objects)
}

func assemble(objects []string) []byte {
	out := []byte("%PDF-1.4\n")
	var offsets []int
	for n, body := range objects {
		offsets = append(offsets, len(out))
		out = append(out, fmt.Sprintf("%d 0 obj\n%s\nendobj\n", n+1, body)...)
	}
	xref := len(out)
	out = append(out, fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)...)
	for _, offset := range offsets {
		out = append(out, fmt.Sprintf("%010d 00000 n \n", offset)...)
	}
	out = append(out, fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)...)
	return out
}

var (
	sheet3 = makePDF([]string{"Calculus. Sheet 3. Series\nHand in by 29.10 23:59 in the LMS", "Task 1. Prove that the series"})
	scan   = makePDF([]string{"", ""})
)

func write(t *testing.T, name string, data []byte) string {
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPDFLocally(t *testing.T) {
	extract, err := ExtractText(write(t, "s.pdf", sheet3), 10)
	if err != nil || extract.Pages != 2 || extract.First() != "Calculus. Sheet 3. Series\nHand in by 29.10 23:59 in the LMS" {
		t.Fatalf("%q %v", extract.Texts, err)
	}
	if Scanned(extract.First()) || !Scanned("") || !Scanned("Стр. 1") {
		t.Fatal("scanned")
	}
	path := write(t, "scan.pdf", scan)
	if extract, err := ExtractText(path, 1); err != nil || len(extract.Texts) != 1 || extract.Texts[0] != "" {
		t.Fatal(extract, err)
	}
	head, err := Head(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if extract, err := ExtractText(write(t, "head.pdf", head), 5); err != nil || extract.Pages != 1 {
		t.Fatal(extract, err)
	}
	if _, err := ExtractText(write(t, "x.pdf", []byte("not a pdf")), 1); err == nil {
		t.Fatal("read a non-PDF")
	} else if _, ok := err.(*PdfError); !ok {
		t.Fatal(err)
	}
	if Diff("a\nb\nc", "a\nB\nc", 100) != "-b\n+B" || Diff("same", "same", 100) != "" ||
		!strings.HasSuffix(Diff("", strings.Repeat("x\n", 100), 10), "…") {
		t.Fatal("diff")
	}
}

// D79: a stream that inflates past the limit is no PDF to read.
func TestABombIsRefused(t *testing.T) {
	var buf bytes.Buffer
	w := newZlib(&buf)
	chunk := bytes.Repeat([]byte("0"), 1<<20)
	for range StreamMax>>20 + 2 {
		w.Write(chunk)
	}
	w.Close()
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", buf.Len(), buf.String())}
	if _, err := ExtractText(write(t, "bomb.pdf", assemble(objects)), 1); err == nil {
		t.Fatal("read a bomb")
	}
}

func TestSimilarNames(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{{"hw5.pdf", "hw5_v2_final.pdf", true}, {"ДЗ3.pdf", "ДЗ3_исправленный (1).pdf", true},
		{"Листок 3 ряды.pdf", "листок 3 ряды новый.pdf", true}, {"Sheet_2.1.pdf", "sheet-2.1-upd.pdf", true},
		{"hw5.pdf", "hw6.pdf", false}, {"matan_list3.pdf", "algebra_list3.pdf", false}} {
		if SimilarNames(c.a, c.b) != c.same {
			t.Error(c.a, c.b)
		}
	}
	if NameKey("hw5_v2_final (1).pdf") != "hw5" || NameKey("дзv2.pdf") != "дзv2" {
		t.Fatal(NameKey("hw5_v2_final (1).pdf"), NameKey("дзv2.pdf"))
	}
}

// A scan's page: its one JPEG as it is stored; a page of text has none.
func TestFirstPageJPEG(t *testing.T) {
	jpeg := append(append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 64)...), 0xff, 0xd9)
	content := "q 1 0 0 1 0 0 cm /Im1 Do Q"
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Resources << /XObject << /Im1 4 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 "+
			"/Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", len(jpeg), jpeg),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)}
	got, err := FirstPageJPEG(write(t, "scan.pdf", assemble(objects)))
	if err != nil || !bytes.Equal(got, jpeg) {
		t.Fatal(got, err)
	}
	if got, err := FirstPageJPEG(write(t, "text.pdf", makePDF([]string{"Problem sheet 1"}))); err != nil || got != nil {
		t.Fatal(got, err)
	}
}
