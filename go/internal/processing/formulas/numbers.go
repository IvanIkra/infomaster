package formulas

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

var (
	decimal  = regexp.MustCompile(`^[+-]?(?:[0-9](?:_?[0-9])*(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)
	infinity = regexp.MustCompile(`(?i)^[+-]?(?:inf|infinity|nan)$`)
)

// PyFloat is Python's float() of a text: surrounding whitespace, "_" between digits, inf and nan in any case. Only
// ASCII digits are taken (Python also reads other decimal digits).
func PyFloat(text string) (float64, bool) {
	text = pytext.Strip(text)
	if infinity.MatchString(text) {
		negative := strings.HasPrefix(text, "-")
		if strings.Contains(strings.ToLower(text), "nan") {
			return math.NaN(), true
		}
		if negative {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	if !decimal.MatchString(text) {
		return 0, false
	}
	x, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return x, true // out of range: ±inf or ±0, as Python has it
}

// Round is Python's round(x, digits) for digits >= 0: the exact binary value rounded half to even.
func Round(x float64, digits int) float64 {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return x
	}
	y, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', digits, 64), 64)
	if y == 0 {
		return math.Copysign(0, x)
	}
	return y
}

// Sum is Python 3.12's sum() of floats: compensated (Neumaier), as the sheets' values were added in Python.
func Sum(xs []float64) float64 {
	result, c := 0.0, 0.0
	for _, x := range xs {
		t := result + x
		if math.Abs(result) >= math.Abs(x) {
			c += (result - t) + x
		} else {
			c += (x - t) + result
		}
		result = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		result += c
	}
	return result
}

// Repr is Python's repr (and str) of a float: 5.9, 8.0, 1e-05, inf.
func Repr(x float64) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	return pyjson.FormatFloat(x)
}

// NumberText is a number in a formula written out: 7 for a whole one, else %g (six significant digits).
func NumberText(x float64) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	case x == 0:
		return "0"
	case x == math.Trunc(x):
		return strconv.FormatFloat(x, 'f', 0, 64)
	}
	return strconv.FormatFloat(x, 'g', 6, 64)
}
