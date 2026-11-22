package formulas

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// Stage 17.6: the formulas of the own row (names and texts made up).

func value(t *testing.T, formula string, cells map[string]Value) (Value, error) {
	t.Helper()
	tree, err := Parse(formula)
	if err != nil {
		t.Fatalf("%s: %v", formula, err)
	}
	return Evaluate(tree, func(tab, col string, row int) (Value, error) {
		key := col + itoa(row)
		if tab != "" {
			key = tab + "!" + key
		}
		return cells[key], nil
	})
}

func itoa(n int) string { return NumberText(float64(n)) }

func must(t *testing.T, formula string, cells map[string]Value, want Value) {
	t.Helper()
	got, err := value(t, formula, cells)
	if err != nil {
		t.Fatalf("%s: %v", formula, err)
	}
	if x, ok := want.(float64); ok {
		if y, ok := got.(float64); !ok || math.Abs(x-y) > 1e-9 {
			t.Fatalf("%s = %#v, want %v", formula, got, want)
		}
		return
	}
	if got != want {
		t.Fatalf("%s = %#v, want %#v", formula, got, want)
	}
}

func TestTheFormulasOfTheGradebooksCountAsExcelDoes(t *testing.T) {
	must(t, "=0.35*A1 + 0.35*B1 + 0.3*C1", map[string]Value{"A1": 10.0, "B1": 8.0, "C1": 6.0}, 8.1)
	must(t, "=ROUND(2.5,0)", nil, 3.0)
	must(t, "=ROUND(-2.5,0)", nil, -3.0)
	must(t, "=ROUND(0.125,2)", nil, 0.13)
	must(t, "=ROUNDUP(5.001/21*10,2)", nil, 2.39)
	must(t, "=ROUNDDOWN(7.99,0)", nil, 7.0)
	// the bonus problems are the ones whose header ends with a star: "*~*"
	row := map[string]Value{"C2": "1а", "D2": "1б", "E2": "2*", "C3": 1.0, "D3": 0.5, "E3": 1.0}
	must(t, `=SUMIF($C$2:$E$2,"*~*",C3:E3)`, row, 1.0)
	must(t, `=sum(C3:E3)-SUMIF($C$2:$E$2,"*~*",C3:E3)`, row, 1.5)
	must(t, `=SUMIF(C3:E3,">=1")`, row, 2.0)
	// blanks: no average, no count, and an IFERROR or IF gives the sheet's ""
	must(t, `=IFERROR(ROUND(AVERAGE(A1,B1),2),"")`, nil, "")
	must(t, `=IF(COUNT(A1:C1)=0,"",MIN(10,A1/10))`, map[string]Value{"B1": 3.0}, 0.0)
	must(t, `=IF(AND($A5<>"",$B5<>""),SUM(C5:D5),"")`, map[string]Value{"A5": "x", "B5": "y", "C5": 2.0, "D5": 3.0}, 5.0)
	// a retake replaces the exam when there is one
	retake := `=IF(OR(ISNUMBER(D1),ISNUMBER(E1)),IF(ISNUMBER(E1),E1,D1)*0.5,"")`
	must(t, retake, map[string]Value{"D1": 4.0}, 2.0)
	must(t, retake, map[string]Value{"D1": 4.0, "E1": 8.0}, 4.0)
	must(t, retake, nil, "")
	must(t, "='ДЗ-1А'!L59 + 'It''s'!A1", map[string]Value{"ДЗ-1А!L59": 2.0, "It's!A1": 1.0}, 3.0)
	must(t, "=1/3", nil, 1.0/3)
	must(t, "=-A1^2", map[string]Value{"A1": 3.0}, 9.0)
	must(t, "=50%", nil, 0.5)
	must(t, `=A1&"x"&TRUE`, map[string]Value{"A1": 1.0}, "1.0xTrue")
	must(t, `="b">"A"`, nil, true)
	must(t, `=A1<"a"`, nil, true) // an empty cell is "" next to a text
	must(t, `=1<"a"`, nil, true)  // numbers before text
	if _, err := value(t, "=A1/B1", map[string]Value{"A1": 1.0}); !isCellError(err) {
		t.Fatal(err)
	}
	for _, unknown := range []string{"=VLOOKUP(A1,B1:C9,2)", "=A:A", "=SUM(A1", "not a formula"} {
		if _, err := Parse(unknown); !isUnsupported(err) {
			t.Fatal(unknown, err)
		}
	}
}

func isCellError(err error) bool {
	var e *CellError
	return errors.As(err, &e)
}

func isUnsupported(err error) bool {
	var e *Unsupported
	return errors.As(err, &e)
}

func TestAFormulaIsWrittenWithTheNamesOfItsCells(t *testing.T) {
	names := map[string]string{"C": "КР1", "D": "КР2", "F": "ДЗ"}
	tree, _ := Parse("=ROUND(0.35*C2+0.35*D2+0.3*F2,0)")
	if got := Render(tree, func(n *Node) string { return names[n.Col] }); got != "ROUND(0.35·КР1 + 0.35·КР2 + 0.3·ДЗ; 0)" {
		t.Fatal(got)
	}
	tree, _ = Parse("=10*(A1+B1)/6")
	if got := Render(tree, func(n *Node) string { return n.Col }); got != "10·(A + B) / 6" {
		t.Fatal(got)
	}
	tree, _ = Parse(`=IF(A1<>"",-(B1-C1)^2,1.5E-5)>=0`)
	if got := Render(tree, func(n *Node) string { return n.Col }); got != "IF(A ≠ «»; −(B − C) ^ 2; 1.5e-05) ≥ 0" {
		t.Fatal(got)
	}
}

// D79: a huge range, a deep nesting, a long formula are not counted; odd arithmetic is an error of the cell.
func TestATeachersFormulaCannotExhaustTheService(t *testing.T) {
	for _, formula := range []string{"=SUM(A1:ZZZ1000000)", "=SUM(A1:A2000000)",
		"=" + strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200), "=" + strings.Repeat("-", 200) + "1",
		"=1" + strings.Repeat("+1", 3000)} {
		if _, err := Parse(formula); !isUnsupported(err) {
			t.Fatal(formula[:20], err)
		}
	}
	var long []string
	for n := 1; n < 400; n++ {
		long = append(long, "A"+itoa(n))
	}
	if _, err := Parse("=" + strings.Join(long, "+")); err != nil { // a long sum is fine
		t.Fatal(err)
	}
	for _, formula := range []string{"=(-8)^0.5", "=10^400", `=ROUND(1;"nan")`, "=1/0", "=0^-1"} {
		if _, err := value(t, formula, nil); !isCellError(err) {
			t.Fatal(formula, err)
		}
	}
	must(t, "=ROUND(1;-400)", nil, 0.0)
	must(t, "=ROUND(2.5;400)", nil, 2.5)
}

func TestTokensBacktrackAsTheRegexDoes(t *testing.T) {
	for formula, want := range map[string]float64{
		"=A1:B2":        0, // a range alone is #VALUE!, checked below
		`=SUM(A1:B1)`:   3,
		`=SUM(A1,B1)`:   3,
		`=SUM(A1;B1)`:   3,
		`=Лист1!A1+1`:   3,
		"=_xlfn.SUM(1)": 1,
		"=1.5e1":        15,
		"=.5+1.":        1.5,
	} {
		got, err := value(t, formula, map[string]Value{"A1": 1.0, "B1": 2.0, "Лист1!A1": 2.0})
		if formula == "=A1:B2" {
			if !isCellError(err) {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || got != want {
			t.Fatal(formula, got, err)
		}
	}
	if _, err := Parse(`="a""`); err != nil { // `"a"` then a stray quote
		if !isUnsupported(err) {
			t.Fatal(err)
		}
	}
	tree, err := Parse(`="say ""hi"""`)
	if err != nil || tree.Str != `say "hi"` {
		t.Fatal(tree, err)
	}
}

func TestPythonNumbers(t *testing.T) {
	for text, want := range map[string]float64{" 5 ": 5, "1_000.5": 1000.5, "-.5": -0.5, "1e400": math.Inf(1),
		"-Infinity": math.Inf(-1)} {
		if got, ok := PyFloat(text); !ok || got != want {
			t.Error(text, got)
		}
	}
	for _, text := range []string{"", "1__0", "_1", "1_", "0x10", "1,5", "abc"} {
		if _, ok := PyFloat(text); ok {
			t.Error(text)
		}
	}
	if Round(0.125, 2) != 0.12 || Round(2.675, 2) != 2.67 || Round(8.425, 2) != 8.43 {
		t.Fatal("round")
	}
	xs := make([]float64, 10)
	for i := range xs {
		xs[i] = 0.1
	}
	if Sum(xs) != 1.0 {
		t.Fatal(Sum(xs))
	}
	for x, want := range map[float64]string{0.5: "0.5", 1e-05: "1e-05", 123456.7: "123457", 1e6: "1000000",
		100000.5: "100000", 0.30000000000000004: "0.3", 1234567.5: "1.23457e+06"} {
		if got := NumberText(x); got != want {
			t.Error(x, got)
		}
	}
	if Repr(5.9) != "5.9" || Repr(8) != "8.0" || Repr(1e-7) != "1e-07" || Repr(math.Inf(1)) != "inf" {
		t.Fatal("repr")
	}
	if ColIndex("AA") != 26 || ColName(26) != "AA" || ColName(0) != "A" || ColName(701) != "ZZ" {
		t.Fatal("columns")
	}
}
