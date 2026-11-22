// Package pyjson writes JSON the way Python's json.dumps does, so text the Python service stored (and compares or
// hashes) stays the same: ", " and ": " between items, no HTML escaping, non-ASCII kept (ensure_ascii=False) or
// escaped as \uXXXX (ASCII).
package pyjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Dumps is json.dumps(value, ensure_ascii=False).
func Dumps(value any) (string, error) { return encode(value, false, ", ", ": ") }

// DumpsASCII is json.dumps(value): non-ASCII escaped as \uXXXX.
func DumpsASCII(value any) (string, error) { return encode(value, true, ", ", ": ") }

// Compact is json.dumps(value, separators=(",", ":"), ensure_ascii=False).
func Compact(value any) (string, error) { return encode(value, false, ",", ":") }

// CompactASCII is json.dumps(value, separators=(",", ":")).
func CompactASCII(value any) (string, error) { return encode(value, true, ",", ":") }

// MustDumps is Dumps for values that always encode (slices of ints, plain maps).
func MustDumps(value any) string {
	text, err := Dumps(value)
	if err != nil {
		panic(err)
	}
	return text
}

// Unmarshal decodes JSON, keeping numbers as float64 like encoding/json.
func Unmarshal(data []byte, out any) error { return json.Unmarshal(data, out) }

func encode(value any, ascii bool, itemSep, keySep string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	compact := bytes.TrimRight(buf.Bytes(), "\n")
	return reformat(compact, ascii, itemSep, keySep)
}

// reformat walks compact JSON and puts Python's separators between items, escaping non-ASCII when asked.
func reformat(src []byte, ascii bool, itemSep, keySep string) (string, error) {
	var out strings.Builder
	inString := false
	for i := 0; i < len(src); {
		c := src[i]
		if inString {
			switch {
			case c == '\\':
				out.WriteByte(c)
				out.WriteByte(src[i+1])
				// Go writes   and   escaped; Python writes them as they are unless ascii.
				if src[i+1] == 'u' && !ascii {
					code := string(src[i+2 : i+6])
					if code == "2028" || code == "2029" {
						s := out.String()
						out.Reset()
						out.WriteString(s[:len(s)-2])
						if code == "2028" {
							out.WriteString(" ")
						} else {
							out.WriteString(" ")
						}
						i += 6
						continue
					}
				}
				i += 2
				continue
			case c == '"':
				inString = false
				out.WriteByte(c)
				i++
				continue
			case c >= utf8.RuneSelf && ascii:
				r, size := utf8.DecodeRune(src[i:])
				if r >= 0x10000 {
					r -= 0x10000
					fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
				} else {
					fmt.Fprintf(&out, "\\u%04x", r)
				}
				i += size
				continue
			default:
				out.WriteByte(c)
				i++
				continue
			}
		}
		switch c {
		case '"':
			inString = true
			out.WriteByte(c)
		case ',':
			out.WriteString(itemSep)
		case ':':
			out.WriteString(keySep)
		default:
			out.WriteByte(c)
		}
		i++
	}
	return out.String(), nil
}

// Float is a float64 that JSON-encodes as Python's repr does: 8.0, not 8. Use it for float fields of stored or
// served JSON (grades, money).
type Float float64

// MarshalJSON writes the float as Python would.
func (f Float) MarshalJSON() ([]byte, error) { return []byte(FormatFloat(float64(f))), nil }

// FormatFloat is Python's repr of a float.
func FormatFloat(f float64) string {
	abs := math.Abs(f)
	if abs >= 1e16 || (abs < 1e-4 && f != 0) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}
