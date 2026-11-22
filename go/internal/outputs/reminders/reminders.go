// Package reminders sends the deadline reminders (D47), as Web Push (D69): the port of outputs/reminders.py.
//
// Twice a day, Moscow time: on the eve (`reminder_eve`, 20:00 by default) the deadlines due tomorrow, in the morning
// (`reminder_day`, 09:00) those due today, as the merged list has them (processing/merge: hidden and done ones are not
// there). One notification holds all of a day's deadlines; nothing due, none. `deadline_reminders` off: none at all.
//
// Each slot is sent once: kv "deadlines.reminded" keeps the day of the last one sent. The very first start takes the
// slots that have come as sent, so a new install floods nothing; a slot missed while the service was down is sent when
// it starts again, the same day.
package reminders

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// DoneKey is slot name -> the Moscow day of the last reminder sent.
const DoneKey = "deadlines.reminded"

// Slot is a reminder: its name, its time setting and the days ahead it looks.
type Slot struct {
	Name, Setting string
	Ahead         int
}

// Slots are the morning's and the eve's.
var Slots = []Slot{{"day", "reminder_day", 0}, {"eve", "reminder_eve", 1}}

// SlotDelay is how long past the slot's minute it runs.
const SlotDelay = 5 * time.Second

// RetryAfter is the time to the next look after a failure.
const RetryAfter = 600 * time.Second

// Reminders is the loop.
type Reminders struct {
	store    *store.Store
	Merged   *merge.DeadlineList
	Settings *settings.Settings
	Now      func() time.Time
	// Notify is the web app's notifier (§8.6): the slot's name and the deadlines due.
	Notify func(name string, due []todo.Deadline)

	woken chan struct{}
	// tick and wait are replaced in the tests; wait reports false when the loop must stop.
	tick func(ctx context.Context) error
	wait func(ctx context.Context, d time.Duration) bool
}

// New is the reminders of a merged list.
func New(s *store.Store, merged *merge.DeadlineList, st *settings.Settings) *Reminders {
	r := &Reminders{store: s, Merged: merged, Settings: st, Now: clock.NowUTC, woken: make(chan struct{}, 1)}
	r.tick = r.Tick
	r.wait = r.sleep
	return r
}

// Reschedule is called when a time changed: the loop counts its sleep anew.
func (r *Reminders) Reschedule() {
	select {
	case r.woken <- struct{}{}:
	default:
	}
}

// --- the loop ------------------------------------------------------------------------------

func (r *Reminders) done() (map[string]string, error) {
	done := map[string]string{}
	_, err := r.store.GetJSON(DoneKey, &done)
	if done == nil {
		done = map[string]string{}
	}
	return done, err
}

// putDone writes the slots; the map's keys come sorted, "day" before "eve", as Python's first start writes them.
func (r *Reminders) putDone(done map[string]string) error { return r.store.PutJSON(DoneKey, done) }

// Start takes the slots that have come as sent on the very first start: a new install floods nothing.
func (r *Reminders) Start() error {
	done, err := r.done()
	if err != nil {
		return err
	}
	for _, slot := range Slots {
		if _, ok := done[slot.Name]; !ok {
			at, err := r.slot(slot.Setting)
			if err != nil {
				return err
			}
			done[slot.Name] = at.Format(clock.DateLayout)
		}
	}
	return r.putDone(done)
}

// Run sends the reminders until ctx is done.
func (r *Reminders) Run(ctx context.Context) {
	if err := r.Start(); err != nil {
		slog.Error("starting the deadline reminders failed", "error", err)
	}
	for {
		select { // what woke it before this look is taken in by it
		case <-r.woken:
		default:
		}
		var wait time.Duration
		err := r.tick(ctx)
		if err == nil {
			wait, err = r.toNext()
		}
		if err != nil { // a broken setting or store: the loop lives on and looks again later
			slog.Error("the deadline reminders failed", "error", err)
			wait = RetryAfter
		}
		if !r.wait(ctx, wait) {
			return
		}
	}
}

func (r *Reminders) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.woken:
	case <-timer.C:
	}
	return true
}

// Tick sends every slot that has come and was not sent; a slot is sent at most once whatever happens.
func (r *Reminders) Tick(ctx context.Context) error {
	done, err := r.done()
	if err != nil {
		return err
	}
	for _, slot := range Slots {
		at, err := r.slot(slot.Setting)
		if err != nil {
			return err
		}
		day := at.Format(clock.DateLayout)
		if day <= done[slot.Name] {
			continue
		}
		done[slot.Name] = day
		if err := r.putDone(done); err != nil { // first: a failing post is not sent twice
			return err
		}
		on, err := r.Settings.Bool("deadline_reminders")
		if err != nil {
			return err
		}
		if !on {
			continue
		}
		due := todo.Date(at.Year(), at.Month(), at.Day()).AddDate(0, 0, slot.Ahead)
		if _, err := r.Remind(slot.Name, due); err != nil {
			slog.Error("a deadline reminder failed", "slot", slot.Name, "error", err)
		}
	}
	return nil
}

// slot is the latest time of the setting that has come, Moscow local.
func (r *Reminders) slot(setting string) (time.Time, error) {
	local := r.Now().In(clock.TZ)
	value, err := r.Settings.String(setting)
	if err != nil {
		return time.Time{}, err
	}
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil {
		return time.Time{}, fmt.Errorf("the setting %s is not a time: %q", setting, value)
	}
	today := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, clock.TZ)
	if !local.Before(today) {
		return today, nil
	}
	return today.AddDate(0, 0, -1), nil
}

func (r *Reminders) toNext() (time.Duration, error) {
	local := r.Now().In(clock.TZ)
	var least time.Duration
	for i, slot := range Slots {
		at, err := r.slot(slot.Setting)
		if err != nil {
			return 0, err
		}
		if wait := at.AddDate(0, 0, 1).Sub(local); i == 0 || wait < least {
			least = wait
		}
	}
	return least + SlotDelay, nil
}

// --- the reminder ---------------------------------------------------------------------------

// Remind tells the notifier the deadlines due that day (a todo date); none, nothing is told.
func (r *Reminders) Remind(name string, due time.Time) ([]todo.Deadline, error) {
	shown, err := r.Merged.Visible()
	if err != nil {
		return nil, err
	}
	var then []todo.Deadline
	for _, d := range shown {
		if todo.DueText(d.Due) == todo.DueText(due) {
			then = append(then, d)
		}
	}
	if len(then) > 0 && r.Notify != nil {
		r.Notify(name, then)
	}
	return then, nil
}
