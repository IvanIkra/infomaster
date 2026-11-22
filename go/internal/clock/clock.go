// Package clock holds the time rules (§9): the database keeps Moscow local time without a zone, except llm_runs.at,
// which is UTC. Both formats match Python's isoformat(timespec="seconds"), so string comparison orders them.
package clock

import (
	"time"
	_ "time/tzdata" // the zone data does not depend on the image's
)

// TZ is Moscow.
var TZ = mustZone("Europe/Moscow")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Now is the current moment; tests replace it.
var Now = time.Now

// NowUTC is the current moment in UTC.
func NowUTC() time.Time { return Now().UTC() }

// LocalDay is the Moscow date of a moment, at midnight in TZ: days, weeks and the LLM limit are counted by it.
func LocalDay(moment time.Time) time.Time {
	m := moment.In(TZ)
	return time.Date(m.Year(), m.Month(), m.Day(), 0, 0, 0, 0, TZ)
}

// DayStartUTC is the Moscow midnight that starts the day of `day` (its date is read in TZ), as UTC.
func DayStartUTC(day time.Time) time.Time { return LocalDay(day).UTC() }

// UTCISO is one format for every UTC column: 2026-11-22T10:00:00+00:00.
func UTCISO(moment time.Time) string { return moment.UTC().Format("2006-01-02T15:04:05") + "+00:00" }

// LocalISO is Moscow local time without a zone: 2026-11-22T13:00:00, the format of every column but llm_runs.at.
func LocalISO(moment time.Time) string { return moment.In(TZ).Format(LocalLayout) }

// LocalLayout is the layout of LocalISO.
const LocalLayout = "2006-01-02T15:04:05"

// DateLayout is the layout of due dates: YYYY-MM-DD.
const DateLayout = "2006-01-02"

// ParseLocal reads a Moscow local time as Python's datetime.fromisoformat reads the stored forms: with seconds, or
// minutes only, or with fractions of a second.
func ParseLocal(text string) (time.Time, error) {
	for _, layout := range []string{LocalLayout, "2006-01-02T15:04", "2006-01-02T15:04:05.999999", DateLayout} {
		if t, err := time.ParseInLocation(layout, text, TZ); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &time.ParseError{Layout: LocalLayout, Value: text}
}

// ParseUTC reads a UTCISO string (or any RFC 3339 time).
func ParseUTC(text string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
