package llm

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Run is one row of llm_runs: every call, also a failed one, with tokens, cost and duration (§7, D22).
type Run struct {
	At           string       `json:"at"` // UTC, clock.UTCISO
	Task         string       `json:"task"`
	ChatID       *int64       `json:"chat_id"`
	Model        string       `json:"model"`
	OK           bool         `json:"ok"`
	Error        *string      `json:"error"`
	InputTokens  int64        `json:"input_tokens"`
	OutputTokens int64        `json:"output_tokens"`
	CacheWrite   int64        `json:"cache_write"`
	CacheRead    int64        `json:"cache_read"`
	CostUSD      pyjson.Float `json:"cost_usd"`
	DurationS    pyjson.Float `json:"duration_s"`
}

const runColumns = "at, task, chat_id, model, ok, error, input_tokens, output_tokens, cache_write, cache_read, " +
	"cost_usd, duration_s"

// Journal is the journal of LLM calls.
type Journal struct {
	db *sql.DB
}

// NewJournal opens the journal.
func NewJournal(s *store.Store) *Journal { return &Journal{db: s.DB} }

// Add writes a run.
func (j *Journal) Add(run Run) error {
	ok := 0
	if run.OK {
		ok = 1
	}
	_, err := j.db.Exec("insert into llm_runs("+runColumns+") values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		run.At, run.Task, run.ChatID, run.Model, ok, run.Error, run.InputTokens, run.OutputTokens, run.CacheWrite,
		run.CacheRead, float64(run.CostUSD), float64(run.DurationS))
	return err
}

// Runs are the newest runs first, of one task when task is not "".
func (j *Journal) Runs(limit int, task string) ([]Run, error) {
	where, args := "", []any{}
	if task != "" {
		where, args = "where task = ?", []any{task}
	}
	rows, err := j.db.Query("select "+runColumns+" from llm_runs "+where+" order by id desc limit ?",
		append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		var r Run
		var ok int64
		var cost, duration float64
		if err := rows.Scan(&r.At, &r.Task, &r.ChatID, &r.Model, &ok, &r.Error, &r.InputTokens, &r.OutputTokens,
			&r.CacheWrite, &r.CacheRead, &cost, &duration); err != nil {
			return nil, err
		}
		r.OK, r.CostUSD, r.DurationS = ok != 0, pyjson.Float(cost), pyjson.Float(duration)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// CostSince is the cost of the runs since a moment.
func (j *Journal) CostSince(since time.Time) (float64, error) {
	var cost float64
	err := j.db.QueryRow("select coalesce(sum(cost_usd), 0) cost from llm_runs where at >= ?",
		clock.UTCISO(since)).Scan(&cost)
	return cost, err
}

// CountSince is the calls of a task since a moment, failed ones too: the daily number of images counts them (D12).
func (j *Journal) CountSince(since time.Time, task string) (int, error) {
	var n int
	err := j.db.QueryRow("select count(*) n from llm_runs where at >= ? and task = ?", clock.UTCISO(since),
		task).Scan(&n)
	return n, err
}

// Entry is the runs, failed runs, cost (rounded to 6 places) and tokens of a group.
type Entry struct {
	Runs    int64        `json:"runs"`
	Failed  int64        `json:"failed"`
	CostUSD pyjson.Float `json:"cost_usd"`
	Tokens  int64        `json:"tokens"`
}

// TaskEntry is the Entry of a task.
type TaskEntry struct {
	Task string `json:"task"`
	Entry
}

// ChatEntry is the Entry of a chat.
type ChatEntry struct {
	ChatID int64 `json:"id"`
	Entry
}

// Totals are the runs since a moment, overall and by task and chat, the costliest first (Python's by_task and
// by_chat dicts, in their order; the web app lists them as [{task, ...}] and [{id, title, ...}]).
type Totals struct {
	Entry
	ByTask []TaskEntry `json:"by_task"`
	ByChat []ChatEntry `json:"by_chat"`
}

const sums = "count(*) runs, coalesce(sum(ok = 0), 0) failed, coalesce(sum(cost_usd), 0) cost, " +
	"coalesce(sum(input_tokens + output_tokens + cache_write + cache_read), 0) tokens"

func scanEntry(scan func(...any) error, extra ...any) (Entry, error) {
	var e Entry
	var cost float64
	if err := scan(append(extra, &e.Runs, &e.Failed, &cost, &e.Tokens)...); err != nil {
		return e, err
	}
	e.CostUSD = pyjson.Float(settings.Round(cost, 6))
	return e, nil
}

// Totals are the runs, cost and tokens since a moment, overall and by task and chat (for the web app, §8.4).
func (j *Journal) Totals(since time.Time) (Totals, error) {
	sinceUTC := clock.UTCISO(since)
	var t Totals
	overall, err := scanEntry(j.db.QueryRow("select "+sums+" from llm_runs where at >= ?", sinceUTC).Scan)
	if err != nil {
		return t, err
	}
	t.Entry, t.ByTask, t.ByChat = overall, []TaskEntry{}, []ChatEntry{}
	for _, column := range []string{"task", "chat_id"} {
		rows, err := j.db.Query(fmt.Sprintf("select %[1]s k, %[2]s from llm_runs where at >= ? and %[1]s is not null "+
			"group by %[1]s order by cost desc", column, sums), sinceUTC)
		if err != nil {
			return t, err
		}
		for rows.Next() {
			if column == "task" {
				var task string
				e, err := scanEntry(rows.Scan, &task)
				if err != nil {
					rows.Close()
					return t, err
				}
				t.ByTask = append(t.ByTask, TaskEntry{task, e})
			} else {
				var chat int64
				e, err := scanEntry(rows.Scan, &chat)
				if err != nil {
					rows.Close()
					return t, err
				}
				t.ByChat = append(t.ByChat, ChatEntry{chat, e})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return t, err
		}
	}
	return t, nil
}

// Usage is today, this week (from Monday) and this month, Moscow days.
type Usage struct {
	Today Totals `json:"today"`
	Week  Totals `json:"week"`
	Month Totals `json:"month"`
}

// Usage is the totals of today, this week and this month.
func (j *Journal) Usage(now time.Time) (Usage, error) {
	today := clock.LocalDay(now)
	weekday := (int(today.Weekday()) + 6) % 7 // Monday is 0
	var u Usage
	var err error
	for _, p := range []struct {
		day time.Time
		out *Totals
	}{
		{today, &u.Today},
		{today.AddDate(0, 0, -weekday), &u.Week},
		{time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, clock.TZ), &u.Month},
	} {
		if *p.out, err = j.Totals(clock.DayStartUTC(p.day)); err != nil {
			return u, err
		}
	}
	return u, nil
}

// Day is the cost, runs and failed runs of one Moscow day.
type Day struct {
	Day     string       `json:"day"`
	Runs    int64        `json:"runs"`
	Failed  int64        `json:"failed"`
	CostUSD pyjson.Float `json:"cost_usd"`
}

// Daily is each of the last `days` Moscow days, today last; days without runs too.
func (j *Journal) Daily(now time.Time, days int) ([]Day, error) {
	today := clock.LocalDay(now)
	first := today.AddDate(0, 0, -(days - 1))
	result := make([]Day, days)
	index := map[string]int{}
	costs := make([]float64, days)
	for n := range days {
		day := first.AddDate(0, 0, n).Format(clock.DateLayout)
		result[n] = Day{Day: day}
		index[day] = n
	}
	rows, err := j.db.Query("select at, ok, cost_usd from llm_runs where at >= ?",
		clock.UTCISO(clock.DayStartUTC(first)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var at string
		var ok int64
		var cost float64
		if err := rows.Scan(&at, &ok, &cost); err != nil {
			return nil, err
		}
		moment, err := clock.ParseUTC(at)
		if err != nil {
			return nil, err
		}
		n, found := index[clock.LocalDay(moment).Format(clock.DateLayout)]
		if !found {
			continue
		}
		result[n].Runs++
		if ok == 0 {
			result[n].Failed++
		}
		costs[n] += cost
	}
	for n := range result {
		result[n].CostUSD = pyjson.Float(settings.Round(costs[n], 6))
	}
	return result, rows.Err()
}
