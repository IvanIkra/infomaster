package actions

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

// The forms of Python's date.fromisoformat and datetime.fromisoformat the owner may give: YYYY-MM-DD (or
// YYYYMMDD), then a separator and HH, HH:MM, HH:MM:SS or HH:MM:SS.ffffff (or their basic forms), then an offset.
var (
	isoDate = regexp.MustCompile(`^(\d{4})-?(\d{2})-?(\d{2})$`)
	isoTime = regexp.MustCompile(`^(\d{2})(?::?(\d{2})(?::?(\d{2})(?:[.,](\d{1,9}))?)?)?` +
		`(Z|[+-]\d{2}(?::?\d{2}(?::?\d{2}(?:[.,]\d{1,6})?)?)?)?$`)
	errISO = errors.New("Invalid isoformat string")
)

// fromISODate is date.fromisoformat: the day as midnight UTC (a todo date), an error for anything else.
func fromISODate(text string) (time.Time, error) {
	m := isoDate.FindStringSubmatch(text)
	if m == nil || len(text) != 10 && len(text) != 8 || len(text) == 10 && strings.Count(text, "-") != 2 {
		return time.Time{}, errISO
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	if year < 1 || month < 1 || month > 12 || day < 1 || day > daysIn(year, time.Month(month)) {
		return time.Time{}, errISO
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), nil
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// fromISO is datetime.fromisoformat: a naive moment (Moscow, as the owner means it) and whether it had an offset.
func fromISO(text string) (time.Time, bool, error) {
	if !isASCII(text) {
		return time.Time{}, false, errISO
	}
	datePart, timePart := text, ""
	if len(text) > 10 && (text[4] == '-') {
		datePart, timePart = text[:10], text[11:]
	} else if len(text) > 8 && text[4] != '-' {
		datePart, timePart = text[:8], text[9:]
	}
	day, err := fromISODate(datePart)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(text) > len(datePart) && timePart == "" {
		return time.Time{}, false, errISO // a separator and nothing after it
	}
	hour, minute, second, nanos, zoned := 0, 0, 0, 0, false
	if timePart != "" {
		m := isoTime.FindStringSubmatch(timePart)
		if m == nil {
			return time.Time{}, false, errISO
		}
		hour, _ = strconv.Atoi(m[1])
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		if m[3] != "" {
			second, _ = strconv.Atoi(m[3])
		}
		if m[4] != "" {
			fraction := (m[4] + "000000000")[:9]
			nanos, _ = strconv.Atoi(fraction)
			nanos = nanos / 1000 * 1000 // microseconds, as Python keeps them
		}
		zoned = m[5] != ""
		if hour > 23 || minute > 59 || second > 59 {
			return time.Time{}, false, errISO
		}
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, second, nanos, clock.TZ), zoned, nil
}

func isASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return false
		}
	}
	return true
}

// isoSeconds is isoformat(timespec="seconds") of a naive moment.
func isoSeconds(moment time.Time) string { return moment.Format(clock.LocalLayout) }

// squeeze is " ".join(text.split()).
func squeeze(text string) string { return strings.Join(pytext.Split(text), " ") }

// pyInt is Python's int() of a text: surrounding whitespace, a sign and underscores between digits are allowed.
func pyInt(text string) (int64, error) {
	text = pytext.Strip(text)
	digits := strings.TrimLeft(text, "+-")
	if len(text)-len(digits) > 1 || digits == "" || strings.HasPrefix(digits, "_") ||
		strings.HasSuffix(digits, "_") || strings.Contains(digits, "__") {
		return 0, errISO
	}
	return strconv.ParseInt(strings.ReplaceAll(text, "_", ""), 10, 64)
}

// FromISODate is Python's date.fromisoformat: YYYY-MM-DD or YYYYMMDD, the day as midnight UTC.
func FromISODate(text string) (time.Time, error) { return fromISODate(text) }
