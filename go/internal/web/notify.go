package web

// The notifier (§8.6, stage 16.4): Web Push to the web app's devices, the service's only notifications (D69). The
// port of web/notify.py.
//   - a new or moved deadline (outputs/deadlines);
//   - a reminder: the deadlines due tomorrow in the evening, today in the morning (D47);
//   - a failure of a source or a login it needs (an Alert: the twin, HSE App X, mail, RUZ, the wiki, a grade sheet);
//   - the daily LLM limit spent (the 80 % warning goes quietly).
//
// A message names its screen in the app: a deadline's card, the feed, «Machine». A deadline's messages share a tag,
// so a move replaces the news of it on the device instead of adding another.
//
// The owner chooses in «Machine → Notifications» (D65) which kinds come (`push_kinds`: deadlines, reminders,
// failures, llm) and the quiet hours (`push_quiet`, Moscow time): a message of those hours comes without a sound or
// vibration.

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

var weekdays = map[string][7]string{
	"en": {"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
	"ru": {"пн", "вт", "ср", "чт", "пт", "сб", "вс"},
}

// BodyChars is the longest body of a notification, in characters.
const BodyChars = 400

// Day is '2026-10-22' as 'Thu 22.10' (the date is day.month in both languages).
func Day(due, lang string) string {
	date, err := time.Parse(clock.DateLayout, due)
	if err != nil {
		return due
	}
	names, ok := weekdays[lang]
	if !ok {
		names = weekdays["en"]
	}
	return names[(int(date.Weekday())+6)%7] + " " + date.Format("02.01")
}

// Quiet reports whether Moscow's time of moment falls in the quiet hours 'HH:MM-HH:MM' (over midnight when the end
// is earlier); never for "".
func Quiet(hours string, moment time.Time) bool {
	if hours == "" {
		return false
	}
	start, end, _ := strings.Cut(hours, "-")
	now := moment.In(clock.TZ).Format("15:04")
	if start < end {
		return start <= now && now < end
	}
	return now >= start || now < end
}

// Notifier sends the service's notifications.
type Notifier struct {
	Push *Push
	Lang func() string
	Now  func() time.Time
	// Settings is the service's settings: push_kinds and push_quiet; all kinds, no quiet hours without.
	Settings *settings.Settings

	tasks sync.WaitGroup
}

// NewNotifier is the notifier of a Push; lang nil is English.
func NewNotifier(push *Push, lang func() string) *Notifier {
	if lang == nil {
		lang = func() string { return "en" }
	}
	return &Notifier{Push: push, Lang: lang, Now: clock.NowUTC}
}

// Wait waits for the messages on their way (tests, a stop).
func (n *Notifier) Wait() { n.tasks.Wait() }

// Send sends one message to every device; screen is the app's route after #/ ("" for the feed). A message of a kind
// the owner turned off is not sent; in the quiet hours it comes silent. It does not wait for the push services.
func (n *Notifier) Send(title, body, screen, tag string, urgent bool, kind string) {
	if kind != "" && n.Settings != nil {
		kinds, err := n.Settings.Strings("push_kinds")
		if err == nil && !slices.Contains(kinds, kind) {
			return
		}
	}
	if pytext.Len(body) > BodyChars {
		body = strings.TrimRightFunc(pytext.Cut(body, BodyChars), pytext.IsSpace) + "…"
	}
	message := Message{Title: title, Body: body, URL: "/web/#/" + screen, Tag: tag}
	if n.Settings != nil {
		if hours, err := n.Settings.String("push_quiet"); err == nil && Quiet(hours, n.Now()) {
			message.Silent = true
		}
	}
	urgency := "normal"
	if urgent {
		urgency = "high"
	}
	n.tasks.Add(1)
	go func() {
		defer n.tasks.Done()
		if _, err := n.Push.Send(context.Background(), message, nil, urgency, 0); err != nil {
			log.Printf("a push failed: %v", err)
		}
	}()
}

// --- what is announced ---------------------------------------------------------------------------------------

// Deadline announces a new deadline (was "") or one moved from was (YYYY-MM-DD): outputs/deadlines' Notify.
func (n *Notifier) Deadline(d todo.Deadline, was string) {
	lang := n.Lang()
	when := Day(todo.DueText(d.Due), lang)
	if d.Assumed {
		when += " ≈ " + i18n.Tr(lang, "assumed", "предположительно")
	}
	name := ""
	if d.Name != nil {
		name = *d.Name
	}
	var title, body string
	if was == "" {
		title = i18n.Tr(lang, "New deadline: "+d.Title, "Новый срок: "+d.Title)
		body = when
		if name != "" {
			body += " · " + name
		}
	} else {
		title = i18n.Tr(lang, "Deadline moved: "+d.Title, "Срок перенесён: "+d.Title)
		body = Day(was, lang) + " → " + when
		if name != "" {
			body += " · " + name
		}
	}
	uid := todo.UID(d)
	n.Send(title, body, "d?uid="+uid, "deadline-"+uid, false, "deadlines")
}

// Reminder announces the deadlines due today (name "day", in the morning) or tomorrow ("eve", in the evening):
// outputs/reminders' Notify.
func (n *Notifier) Reminder(name string, deadlines []todo.Deadline) {
	if len(deadlines) == 0 {
		return
	}
	lang := n.Lang()
	title := i18n.Tr(lang, "Due tomorrow", "Завтра срок")
	if name == "day" {
		title = i18n.Tr(lang, "Due today", "Сегодня срок")
	}
	lines := make([]string, 0, len(deadlines))
	for _, d := range deadlines {
		line := d.Title
		if d.Name != nil && *d.Name != "" {
			line += " · " + *d.Name
		}
		lines = append(lines, line)
	}
	n.Send(title, strings.Join(lines, "\n"), "", "reminder-"+name+"-"+todo.DueText(deadlines[0].Due), true,
		"reminders")
}

// System is a line about the service: it is pushed when it is a failure (an Alert) or a login is needed (login is
// the source that needs it, "" for none).
func (n *Notifier) System(text i18n.Phrase, login string) {
	if !text.Alert && login == "" {
		return
	}
	tag := ""
	if login != "" {
		tag = "login-" + login
	}
	n.Send("infomaster", text.Get(n.Lang()), "machine", tag, true, "failures")
}

// Budget announces the daily LLM limit spent: llm.Budget's Notify.
func (n *Notifier) Budget(notice llm.Notice) {
	if notice.Level != llm.Hard {
		return
	}
	lang := n.Lang()
	n.Send(i18n.Tr(lang, "The LLM limit is spent", "Лимит LLM исчерпан"),
		i18n.Tr(lang, fmt.Sprintf("$%.2f of $%.2f today. Calls wait until midnight Moscow time, messages queue up.",
			notice.Spent, notice.Limit),
			fmt.Sprintf("$%.2f из $%.2f за день. Вызовы стоят до полуночи по Москве, сообщения копятся в очереди.",
				notice.Spent, notice.Limit)), "machine/llm", "budget", false, "llm")
}
