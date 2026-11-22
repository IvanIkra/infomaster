// Package pdf reads PDFs locally (§6.3, D14): the text of their pages, the first pages as a PDF of their own for the
// model, the biggest JPEG of a scanned first page, the comparison of two versions. No renderer: a scan goes to the
// model as a PDF of its first pages, which the model reads as page images.
//
// pdfcpu (Apache-2.0) reads the structure, cuts the first pages and holds every stream to StreamMax when inflated
// (D79: the container has 256 MB); ledongthuc/pdf (BSD) reads the text, only after every stream it inflates has
// passed that limit. Everything here is synchronous and may take a while on a big file.
package pdf

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"

	textpdf "github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

const (
	StreamMax = 20 << 20 // what one stream may inflate to
	ScanWords = 5        // a first page with fewer words has no text layer worth the name: a scan
	JPEGMax   = 5 << 20  // bytes of a page image handed out whole
)

// PdfError is a file that is no PDF this package can read: broken, or encrypted with a password.
type PdfError struct{ Msg string }

func (e *PdfError) Error() string { return e.Msg }

// Extract is the text of the first pages read.
type Extract struct {
	Pages int      // in the whole file
	Texts []string // of the first pages read
}

// First is the first page's text.
func (e Extract) First() string {
	if len(e.Texts) == 0 {
		return ""
	}
	return e.Texts[0]
}

// Text is the pages' text, one after another.
func (e Extract) Text() string { return strings.Join(e.Texts, "\n") }

// Scanned reports a first page without a text layer.
func Scanned(firstPage string) bool { return len(pytext.Split(firstPage)) < ScanWords }

var once sync.Once

func config() *model.Configuration {
	once.Do(api.DisableConfigDir) // no configuration folder in the service user's home
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	conf.Offline = true
	conf.Limits.MaxDecodeBytes = StreamMax
	return conf
}

// inflated are the filters the text reader inflates: each such stream is decoded once under the limit first.
var inflated = map[string]bool{"FlateDecode": true, "LZWDecode": true, "ASCII85Decode": true,
	"ASCIIHexDecode": true, "RunLengthDecode": true}

type document struct {
	data []byte
	ctx  *model.Context
}

func open(path string) (*document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ctx, err := readContext(data)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "password") {
			return nil, &PdfError{"the PDF is encrypted with a password"}
		}
		return nil, &PdfError{"cannot read the PDF: " + err.Error()}
	}
	if ctx.PageCount < 1 { // an empty or broken page tree fails here, not later
		return nil, &PdfError{"cannot read the PDF: no pages"}
	}
	for _, entry := range ctx.Table {
		if entry == nil || entry.Free {
			continue
		}
		sd, ok := entry.Object.(types.StreamDict)
		if !ok || len(sd.FilterPipeline) == 0 {
			continue
		}
		check := true
		for _, f := range sd.FilterPipeline {
			check = check && inflated[f.Name]
		}
		if !check {
			continue
		}
		if err := ctx.XRefTable.DecodeStream(&sd); err != nil {
			return nil, &PdfError{"cannot read the PDF: " + err.Error()}
		}
	}
	return &document{data, ctx}, nil
}

func readContext(data []byte) (ctx *model.Context, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	ctx, err = api.ReadContext(context.Background(), bytes.NewReader(data), config())
	if err != nil {
		return nil, err
	}
	if err := ctx.EnsurePageCount(); err != nil {
		return nil, err
	}
	return ctx, nil
}

var blanks = regexp.MustCompile("[ \t ]+")

// clean is the lines without trailing blanks, no empty runs.
func clean(text string) string {
	var lines []string
	for _, line := range splitLines(text) {
		if line = pytext.Strip(blanks.ReplaceAllString(line, " ")); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func splitLines(text string) []string {
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

func pageText(r *textpdf.Reader, n int) (text string) {
	defer func() {
		if recover() != nil {
			text = "" // a page that fails to give its text gives ""
		}
	}()
	var out strings.Builder
	first, lastY := true, 0.0
	for _, piece := range r.Page(n).Content().Text { // a new line where the baseline moves
		if !first && math.Abs(piece.Y-lastY) > 1 {
			out.WriteByte('\n')
		}
		out.WriteString(piece.S)
		first, lastY = false, piece.Y
	}
	lines := []string{out.String()}
	return clean(strings.Join(lines, "\n"))
}

// ExtractText is the text of the first pages; a page that fails to give its text gives "".
func ExtractText(path string, pages int) (Extract, error) {
	doc, err := open(path)
	if err != nil {
		return Extract{}, err
	}
	count := doc.ctx.PageCount
	texts := []string{}
	reader, err := newTextReader(doc.data)
	for n := 1; n <= min(pages, count); n++ {
		if err != nil {
			texts = append(texts, "")
			continue
		}
		texts = append(texts, pageText(reader, n))
	}
	return Extract{Pages: count, Texts: texts}, nil
}

func newTextReader(data []byte) (r *textpdf.Reader, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	return textpdf.NewReader(bytes.NewReader(data), int64(len(data)))
}

// Head is a PDF of the first pages: what the model sees of a scan.
func Head(path string, pages int) ([]byte, error) {
	doc, err := open(path)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	selected := []string{fmt.Sprintf("1-%d", max(1, min(pages, doc.ctx.PageCount)))}
	if err := trim(doc.data, &out, selected); err != nil { // a broken page's resources: no scan attached
		return nil, &PdfError{"cannot copy the PDF's pages: " + err.Error()}
	}
	return out.Bytes(), nil
}

func trim(data []byte, out *bytes.Buffer, selected []string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return api.Trim(context.Background(), bytes.NewReader(data), out, selected, config())
}

// FirstPageJPEG is the biggest JPEG image on the first page, as it is stored: what a scanned page is. A scanner
// puts each page in as one JPEG, which needs no decoding. Nil for a page without one.
func FirstPageJPEG(path string) ([]byte, error) {
	doc, err := open(path)
	if err != nil {
		return nil, err
	}
	return firstJPEG(doc.ctx), nil
}

func firstJPEG(ctx *model.Context) (best []byte) {
	defer func() {
		if recover() != nil {
			best = nil
		}
	}()
	xref := ctx.XRefTable
	_, _, inherited, err := xref.PageDict(context.Background(), 1, false)
	if err != nil || inherited == nil || inherited.Resources == nil {
		return nil
	}
	images, err := xref.DereferenceDict(inherited.Resources["XObject"])
	if err != nil || images == nil {
		return nil
	}
	for _, ref := range images {
		sd, _, err := xref.DereferenceStreamDict(ref)
		if err != nil || sd == nil {
			continue
		}
		if subtype := sd.Subtype(); subtype == nil || *subtype != "Image" || !dct(sd.Dict["Filter"]) {
			continue
		}
		raw := sd.Raw
		if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xd8 && len(raw) <= JPEGMax && len(raw) > len(best) {
			best = raw
		}
	}
	return best
}

func dct(filter types.Object) bool {
	switch f := filter.(type) {
	case types.Name:
		return f == "DCTDecode"
	case types.Array:
		if len(f) == 1 {
			if n, ok := f[0].(types.Name); ok {
				return n == "DCTDecode"
			}
		}
	}
	return false
}
