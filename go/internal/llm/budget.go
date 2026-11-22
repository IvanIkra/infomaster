package llm

import (
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// The daily LLM limit in dollars for all calls together (§7, D21, D27). Days are Moscow days.
//
// Before a call the worst case is reserved: input estimated from the text length plus max_tokens of output. A
// reserve that does not fit what is left is refused. After the answer the journal holds the real cost and the
// reserve is dropped.
//
//   - 80 % of the limit: optional tasks stop (images, whole PDFs).
//   - 100 %, or a required call that does not fit: every call stops until midnight or until the limit is raised.
//
// Days that stopped are remembered with their limit (kv "llm.days", the last DaysKept days) for the chart of the
// «Machine → LLM» in the web app.

const (
	LongPrompt    = 100_000 // tokens; longer prompts are billed at the long rates
	CacheWrite    = 1.25    // times the input price
	CacheRead     = 0.1     // times the input price
	CharsPerToken = 2       // pessimistic for Russian text: the reserve must not come out below the real cost
	PageTokens    = 3000    // a PDF page given as a document: its image and its text, the high end of the docs
	ImageTokens   = 4800    // an image: the most a high-resolution model takes for one (2576 px on the long edge)
	SoftShare     = 0.8
	DaysKept      = 40
)

// OptionalTasks stop at the soft level.
var OptionalTasks = []string{"image", "pdf"}

// Level is how much of the day's limit is spent.
type Level string

const (
	OK   Level = "ok"
	Soft Level = "soft" // optional tasks are off
	Hard Level = "hard" // every call waits for midnight
)

// Notice is crossing 80 % or stopping, once a day each.
type Notice struct {
	Level Level
	Spent float64
	Limit float64
}

// BudgetExceeded is a call that was not made; the caller keeps its input for later. DoesNotFit: the call alone is
// too big for what is left today; other calls still run. It is not an *Error: callers catch the two apart.
type BudgetExceeded struct {
	Msg        string
	DoesNotFit bool
}

func (e *BudgetExceeded) Error() string { return e.Msg }

// CostUSD is the cost of a call; price is one model's entry of the llm_prices setting, $ per million tokens.
func CostUSD(price settings.Price, inputTokens, outputTokens, cacheWrite, cacheRead int64) float64 {
	long := inputTokens+cacheWrite+cacheRead > LongPrompt
	in, out := float64(price.Input), float64(price.Output)
	if long {
		in, out = float64(price.LongInput), float64(price.LongOutput)
	}
	total := (float64(inputTokens)+float64(cacheWrite)*CacheWrite+float64(cacheRead)*CacheRead)*in +
		float64(outputTokens)*out
	return total / 1_000_000
}

// Estimate is the worst case of a call: the input from its length, the whole max_tokens out.
func Estimate(price settings.Price, inputChars, maxTokens int) float64 {
	return CostUSD(price, int64(math.Ceil(float64(inputChars)/CharsPerToken)), int64(maxTokens), 0, 0)
}

// Budget is the daily limit.
type Budget struct {
	Settings *settings.Settings
	Journal  *Journal
	Store    *store.Store
	Now      func() time.Time
	Notify   func(Notice) // crossing 80 % or stopping, once a day each; called outside the budget's lock

	mu       sync.Mutex
	reserved map[int]float64
	tickets  int
}

// NewBudget makes the budget; Now is clock.NowUTC.
func NewBudget(s *settings.Settings, journal *Journal, st *store.Store) *Budget {
	return &Budget{Settings: s, Journal: journal, Store: st, Now: clock.NowUTC, reserved: map[int]float64{}}
}

// Price is one model's price; ok is false when the setting has none.
func (b *Budget) Price(model string) (settings.Price, bool, error) {
	prices, err := b.Settings.Prices()
	if err != nil {
		return settings.Price{}, false, err
	}
	price, ok := prices[model]
	return price, ok, nil
}

// Limit is today's limit: the setting and what was raised for today.
func (b *Budget) Limit() (float64, error) {
	limit, err := b.Settings.Float("llm_daily_limit_usd")
	if err != nil {
		return 0, err
	}
	raised, err := b.raised()
	return limit + raised, err
}

// Spent is today's real cost plus what calls in flight have reserved.
func (b *Budget) Spent() (float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent()
}

func (b *Budget) spent() (float64, error) {
	cost, err := b.Journal.CostSince(clock.DayStartUTC(b.day()))
	if err != nil {
		return 0, err
	}
	for _, ticket := range sortedTickets(b.reserved) {
		cost += b.reserved[ticket]
	}
	return cost, nil
}

func sortedTickets(reserved map[int]float64) []int {
	tickets := make([]int, 0, len(reserved))
	for t := range reserved {
		tickets = append(tickets, t)
	}
	slices.Sort(tickets)
	return tickets
}

// Level is the level of today.
func (b *Budget) Level() (Level, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.level()
}

func (b *Budget) level() (Level, error) {
	notified, err := b.notified()
	if err != nil {
		return "", err
	}
	if slices.Contains(notified, string(Hard)) {
		return Hard, nil
	}
	spent, err := b.spent()
	if err != nil {
		return "", err
	}
	limit, err := b.Limit()
	if err != nil {
		return "", err
	}
	switch {
	case spent >= limit:
		return Hard, nil
	case spent >= SoftShare*limit:
		return Soft, nil
	}
	return OK, nil
}

// Reserve is a ticket for Release, or *BudgetExceeded. An optional task that does not fit what is left is refused
// alone (DoesNotFit); a required one stops every call until midnight.
func (b *Budget) Reserve(task string, cost float64) (int, error) {
	b.mu.Lock()
	ticket, notice, err := b.reserve(task, cost)
	b.mu.Unlock()
	b.send(notice)
	return ticket, err
}

func (b *Budget) reserve(task string, cost float64) (int, *Notice, error) {
	level, err := b.level()
	if err != nil {
		return 0, nil, err
	}
	stopped := &BudgetExceeded{Msg: "the daily LLM limit is spent, calls wait for midnight"}
	if level == Hard {
		return 0, nil, stopped
	}
	optional := slices.Contains(OptionalTasks, task)
	if optional && level == Soft {
		return 0, nil, &BudgetExceeded{Msg: fmt.Sprintf("optional LLM tasks are off above %.0f%% of the daily limit",
			SoftShare*100)}
	}
	spent, err := b.spent()
	if err != nil {
		return 0, nil, err
	}
	limit, err := b.Limit()
	if err != nil {
		return 0, nil, err
	}
	if spent+cost > limit {
		if optional {
			return 0, nil, &BudgetExceeded{Msg: "the call does not fit what is left of the daily LLM limit",
				DoesNotFit: true}
		}
		notice, err := b.announce(Hard)
		if err != nil {
			return 0, nil, err
		}
		return 0, notice, stopped
	}
	b.tickets++
	b.reserved[b.tickets] = cost
	return b.tickets, nil, nil
}

// Release is called after the call is in the journal: the real cost replaces the reserve.
func (b *Budget) Release(ticket int) error {
	b.mu.Lock()
	notice, err := b.release(ticket)
	b.mu.Unlock()
	b.send(notice)
	return err
}

func (b *Budget) release(ticket int) (*Notice, error) {
	delete(b.reserved, ticket)
	level, err := b.level()
	if err != nil || level == OK {
		return nil, err
	}
	return b.announce(level)
}

// RaiseToday is «Machine»: more money until midnight. It returns today's new limit.
func (b *Budget) RaiseToday(usd float64) (float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raised, err := b.raised()
	if err != nil {
		return 0, err
	}
	if err := b.putToday("llm.raised", pyjson.Float(raised+usd)); err != nil {
		return 0, err
	}
	if err := b.limitChanged(); err != nil {
		return 0, err
	}
	return b.Limit()
}

// LimitChanged is called after the limit of today changed: calls run again if it now leaves room, and reaching it
// is announced again.
func (b *Budget) LimitChanged() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limitChanged()
}

func (b *Budget) limitChanged() error {
	spent, err := b.spent()
	if err != nil {
		return err
	}
	limit, err := b.Limit()
	if err != nil {
		return err
	}
	levels := []string{}
	if spent >= SoftShare*limit {
		levels = []string{string(Soft)}
	}
	return b.putToday("llm.notified", levels)
}

// StoppedDays are the Moscow days (ISO) when every call stopped, with the limit of that day.
func (b *Budget) StoppedDays() (map[string]pyjson.Float, error) {
	days := map[string]pyjson.Float{}
	_, err := b.Store.GetJSON("llm.days", &days)
	return days, err
}

// Status is the budget for the API and the web app: money rounded to 4 places.
type Status struct {
	SpentUSD  pyjson.Float `json:"spent_usd"`
	LimitUSD  pyjson.Float `json:"limit_usd"`
	RaisedUSD pyjson.Float `json:"raised_usd"`
	Level     Level        `json:"level"`
}

// Status is the budget of today.
func (b *Budget) Status() (Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	spent, err := b.spent()
	if err != nil {
		return Status{}, err
	}
	limit, err := b.Limit()
	if err != nil {
		return Status{}, err
	}
	raised, err := b.raised()
	if err != nil {
		return Status{}, err
	}
	level, err := b.level()
	if err != nil {
		return Status{}, err
	}
	return Status{pyjson.Float(settings.Round(spent, 4)), pyjson.Float(settings.Round(limit, 4)),
		pyjson.Float(settings.Round(raised, 4)), level}, nil
}

func (b *Budget) send(notice *Notice) {
	if notice != nil && b.Notify != nil {
		b.Notify(*notice)
	}
}

// announce marks a level as announced today; the notice to send is nil when it already was.
func (b *Budget) announce(level Level) (*Notice, error) {
	notified, err := b.notified()
	if err != nil {
		return nil, err
	}
	if slices.Contains(notified, string(level)) {
		return nil, nil
	}
	reached := []string{string(Soft)}
	if level == Hard {
		reached = append(reached, string(Hard))
	}
	for _, l := range reached {
		if !slices.Contains(notified, l) {
			notified = append(notified, l)
		}
	}
	slices.Sort(notified)
	if err := b.putToday("llm.notified", notified); err != nil {
		return nil, err
	}
	limit, err := b.Limit()
	if err != nil {
		return nil, err
	}
	if level == Hard {
		days, err := b.StoppedDays()
		if err != nil {
			return nil, err
		}
		days[b.day().Format(clock.DateLayout)] = pyjson.Float(settings.Round(limit, 4))
		keys := make([]string, 0, len(days))
		for day := range days {
			keys = append(keys, day)
		}
		slices.Sort(keys)
		if len(keys) > DaysKept {
			keys = keys[len(keys)-DaysKept:]
		}
		kept := make(map[string]pyjson.Float, len(keys)) // JSON writes the keys sorted, as Python's sorted dict
		for _, day := range keys {
			kept[day] = days[day]
		}
		if err := b.Store.PutJSON("llm.days", kept); err != nil {
			return nil, err
		}
	}
	spent, err := b.spent()
	if err != nil {
		return nil, err
	}
	return &Notice{Level: level, Spent: spent, Limit: limit}, nil
}

func (b *Budget) day() time.Time { return clock.LocalDay(b.Now()) }

type today[T any] struct {
	Day   string `json:"day"`
	Value T      `json:"value"`
}

func (b *Budget) raised() (float64, error) {
	var saved today[float64]
	found, err := b.Store.GetJSON("llm.raised", &saved)
	if err != nil || !found || saved.Day != b.day().Format(clock.DateLayout) {
		return 0, err
	}
	return saved.Value, nil
}

func (b *Budget) notified() ([]string, error) {
	var saved today[[]string]
	found, err := b.Store.GetJSON("llm.notified", &saved)
	if err != nil || !found || saved.Day != b.day().Format(clock.DateLayout) {
		return []string{}, err
	}
	return saved.Value, nil
}

func (b *Budget) putToday(key string, value any) error {
	return b.Store.PutJSON(key, map[string]any{"day": b.day().Format(clock.DateLayout), "value": value})
}
