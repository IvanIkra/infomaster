package web

// What the web app reads (§8.6, stage 16.2): one feed of classes and deadlines, the week, an item's card, a course.
// The port of web/views.py.
//
// A course is known by its key: the dictionary's abbreviation of its name (§6.2), or the name itself until the
// dictionary has one. Classes, events and deadlines all name their course, each in its own way; the key is what they
// share, so the course's lens is one filter over all of them.
//
// Deadlines have a date and no time. A study deadline due on a day the course has a class is shown at that class (a
// seminar first): it is handed in there. The others close their day.

import (
	"database/sql"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library"
	"github.com/IvanIkra/infomaster/go/internal/library/links"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/processing/todo"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	FeedDays        = 14  // classes of the feed, from today on; deadlines come however far they are
	PastCourseDays  = 30  // a course of the lenses had a class this recently, or has one ahead
	AheadCourseDays = 60  //
	QuoteChars      = 700 // of a source message on a card
	TwinDays        = 14  // «It is a duplicate…» offers the deadlines this close to the card's
	Twins           = 8
)

// SourceNames are the deadlines' sources a card names by themselves.
var SourceNames = map[string]i18n.Phrase{"hseapp": i18n.P("HSE App X", "HSE App X"),
	"ical": i18n.P("the timetable", "РУЗ"), "ha": i18n.P("added by you", "добавлен вами")}

// Data is what the screens read.
type Data struct {
	Store    *store.Store
	Schedule *schedule.Schedule
	Merged   *merge.DeadlineList
	Library  *library.Library // nil: no library
	Grades   *grades.Grades   // nil: no gradebooks
	Links    *links.Links     // nil: no signed links
	Lang     func() string
	News     *news.News // nil: no news
	TTLHours func() int // how long news lives (the setting news_ttl_hours)
}

// NewData is the screens over the timetable and the merged deadlines; set the optional parts after it.
func NewData(s *store.Store, sch *schedule.Schedule, merged *merge.DeadlineList) *Data {
	return &Data{Store: s, Schedule: sch, Merged: merged, Lang: func() string { return "en" },
		TTLHours: func() int { return 24 }}
}

func (d *Data) lang() string {
	if d.Lang == nil {
		return "en"
	}
	return d.Lang()
}

// --- time and courses ---------------------------------------------------------------------------------------

// Now is Moscow local time, as the timetable keeps it.
func (d *Data) Now() time.Time { return d.Schedule.Now().In(clock.TZ) }

func isoLocal(t time.Time) string   { return t.In(clock.TZ).Format("2006-01-02T15:04:05") }
func isoMinutes(t time.Time) string { return t.In(clock.TZ).Format("2006-01-02T15:04") }
func dateOf(t time.Time) string     { return t.In(clock.TZ).Format(clock.DateLayout) }
func midnight(day string) time.Time {
	t, _ := time.ParseInLocation(clock.DateLayout, day, clock.TZ)
	return t
}

// nullable is nil for "", the text else.
func nullable(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

func deref(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

// Key is the course key of a name: its abbreviation, or the name until the dictionary has one; "" for none.
func (d *Data) Key(name string) (string, error) {
	if name == "" || subjects.Clean(name) == "" {
		return "", nil
	}
	abbr, err := d.Schedule.Subjects.Abbr(name)
	if err != nil || abbr != "" {
		return abbr, err
	}
	return subjects.Clean(name), nil
}

// Title is the course's title for a key: the dictionary's, else the key itself; "" for "".
func (d *Data) Title(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	all, err := d.Schedule.Subjects.All()
	if err != nil {
		return "", err
	}
	for _, s := range all {
		if s.Abbr == key {
			if s.Title != "" {
				return s.Title, nil
			}
			break
		}
	}
	return key, nil
}

// Course is a lens of the courses.
type Course struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// Courses are the lenses: courses with a class in the last month or the next two, the soonest class first.
func (d *Data) Courses() ([]Course, error) {
	now := d.Now()
	since, until := now.AddDate(0, 0, -PastCourseDays), now.AddDate(0, 0, AheadCourseDays)
	classes, err := d.Schedule.ClassEntries()
	if err != nil {
		return nil, err
	}
	soonest := map[string]time.Time{}
	for _, entry := range classes {
		if entry.Cancelled || entry.Start.Before(since) || entry.Start.After(until) {
			continue
		}
		key := entry.Abbr
		if key == "" {
			key = subjects.Clean(entry.Title)
		}
		moment := entry.Start
		if moment.Before(now) {
			moment = moment.AddDate(0, 0, 3650)
		}
		if known, ok := soonest[key]; !ok || moment.Before(known) {
			soonest[key] = moment
		}
	}
	keys := make([]string, 0, len(soonest))
	for key := range soonest {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if !soonest[keys[i]].Equal(soonest[keys[j]]) {
			return soonest[keys[i]].Before(soonest[keys[j]])
		}
		return keys[i] < keys[j]
	})
	out := []Course{}
	for _, key := range keys {
		title, err := d.Title(key)
		if err != nil {
			return nil, err
		}
		out = append(out, Course{key, title})
	}
	return out, nil
}

func (d *Data) eventKeys() (map[string]string, error) {
	inForce, err := d.Schedule.Events.InForceAll()
	if err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for _, e := range inForce {
		if e.ClassUID != nil {
			continue
		}
		key, err := d.Key(deref(e.Course))
		if err != nil {
			return nil, err
		}
		keys[e.UID] = key
	}
	return keys, nil
}

// EntryKey is the course key of a timetable entry: a class's by its course, an event's by what it names.
func (d *Data) EntryKey(entry schedule.Entry, events map[string]string) string {
	if entry.OfClass {
		if entry.Abbr != "" {
			return entry.Abbr
		}
		return subjects.Clean(entry.Title)
	}
	return events[entry.UID]
}

// DeadlineKey is the course key of a deadline.
func (d *Data) DeadlineKey(dl todo.Deadline) (string, error) {
	if dl.Course != nil && *dl.Course != "" {
		return *dl.Course, nil
	}
	return d.Key(deref(dl.Name))
}

// CourseName is a name of the course for a key, as a deadline's row keeps it: the dictionary's, else the
// timetable's; "" for no such course.
func (d *Data) CourseName(key string) (string, error) {
	all, err := d.Schedule.Subjects.All()
	if err != nil {
		return "", err
	}
	for _, s := range all { // ordered by name
		if s.Abbr == key {
			return s.Name, nil
		}
	}
	classes, err := d.Schedule.Ruz.Classes("", "9999")
	if err != nil {
		return "", err
	}
	for _, item := range classes {
		found, err := d.Key(item.Subject)
		if err != nil {
			return "", err
		}
		if found == key {
			return item.Subject, nil
		}
	}
	return "", nil
}

// --- items ---------------------------------------------------------------------------------------------------

// told is a value (a string, an i18n.Phrase or nil) in the app's language; nil for nil.
func (d *Data) told(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case i18n.Phrase:
		return v.Get(d.lang())
	case *i18n.Phrase:
		if v == nil {
			return nil
		}
		return v.Get(d.lang())
	case string:
		return v
	}
	return value
}

// DueRef is a deadline handed in at a class.
type DueRef struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
}

// AtClass is the class a deadline is handed in at.
type AtClass struct {
	UID   string `json:"uid"`
	Start string `json:"start"`
	Title string `json:"title"`
}

// ClassItem is a class or an event of the feed.
type ClassItem struct {
	Kind      string   `json:"kind"`
	UID       string   `json:"uid"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Start     string   `json:"start"`
	End       string   `json:"end"`
	AllDay    bool     `json:"all_day"`
	Place     string   `json:"place"`
	Online    bool     `json:"online"`
	Cancelled bool     `json:"cancelled"`
	OfClass   bool     `json:"of_class"`
	Notes     []string `json:"notes"`
	Course    *string  `json:"course"`
	Past      bool     `json:"past"`
	Due       []DueRef `json:"due"`
}

// DeadlineItem is a deadline of the feed.
type DeadlineItem struct {
	Kind        string   `json:"kind"`
	UID         string   `json:"uid"`
	Title       string   `json:"title"`
	Due         string   `json:"due"`
	Course      *string  `json:"course"`
	CourseTitle *string  `json:"course_title"`
	Category    string   `json:"category"`
	Source      string   `json:"source"`
	From        any      `json:"from"`
	Assumed     bool     `json:"assumed"`
	Overdue     bool     `json:"overdue"`
	AtClass     *AtClass `json:"at_class"`
}

// ClassItem is an entry as the feed shows it.
func (d *Data) ClassItem(entry schedule.Entry, key string, now time.Time) *ClassItem {
	notes := entry.Notes
	if notes == nil {
		notes = []string{}
	}
	return &ClassItem{Kind: "class", UID: entry.UID, Title: entry.Title, Type: entry.Type, Start: isoLocal(entry.Start),
		End: isoLocal(entry.End), AllDay: entry.AllDay, Place: entry.Location,
		Online: strings.HasPrefix(entry.Location, "http"), Cancelled: entry.Cancelled, OfClass: entry.OfClass,
		Notes: notes, Course: nullable(key), Past: !entry.End.After(now), Due: []DueRef{}}
}

// DeadlineItem is a deadline as the feed shows it.
func (d *Data) DeadlineItem(dl todo.Deadline, today string) (*DeadlineItem, error) {
	key, err := d.DeadlineKey(dl)
	if err != nil {
		return nil, err
	}
	var courseTitle *string
	if dl.Course != nil && *dl.Course != "" {
		title, err := d.Title(key)
		if err != nil {
			return nil, err
		}
		courseTitle = nullable(title)
	} else {
		courseTitle = dl.Name
	}
	from, err := d.sourceName(dl)
	if err != nil {
		return nil, err
	}
	due := todo.DueText(dl.Due)
	return &DeadlineItem{Kind: "deadline", UID: todo.UID(dl), Title: dl.Title, Due: due, Course: nullable(key),
		CourseTitle: courseTitle, Category: dl.Category, Source: dl.Source, From: from, Assumed: dl.Assumed,
		Overdue: due < today}, nil
}

func (d *Data) sourceName(dl todo.Deadline) (any, error) {
	if phrase, ok := SourceNames[dl.Source]; ok {
		return d.told(phrase), nil
	}
	row, err := d.row(dl)
	if err != nil || row == nil {
		return nil, err
	}
	told, err := d.Schedule.ToldBy(schedule.OfDeadline(row))
	return d.told(told), err
}

func (d *Data) row(dl todo.Deadline) (*deadlines.Deadline, error) {
	rest, ok := strings.CutPrefix(dl.Ref, "dl:")
	if !ok || rest == "" || strings.TrimFunc(rest, func(r rune) bool { return r >= '0' && r <= '9' }) != "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return nil, nil
	}
	return d.Merged.Deadlines.Get(id)
}

func (d *Data) deadlines(course string) ([]todo.Deadline, error) {
	visible, err := d.Merged.Visible()
	if err != nil {
		return nil, err
	}
	out := []todo.Deadline{}
	for _, dl := range visible {
		if course != "" {
			key, err := d.DeadlineKey(dl)
			if err != nil {
				return nil, err
			}
			if key != course {
				continue
			}
		}
		out = append(out, dl)
	}
	return out, nil
}

type keyed struct {
	entry schedule.Entry
	key   string
}

// entries are the timetable's entries starting in [since, until), of a course's lens unless course is "".
func (d *Data) entries(since, until time.Time, course string) ([]keyed, error) {
	events, err := d.eventKeys()
	if err != nil {
		return nil, err
	}
	all, err := d.Schedule.Entries()
	if err != nil {
		return nil, err
	}
	listed := []keyed{}
	for _, entry := range all {
		if entry.Start.Before(since) || !entry.Start.Before(until) {
			continue
		}
		key := d.EntryKey(entry, events)
		if course == "" || key == course {
			listed = append(listed, keyed{entry, key})
		}
	}
	return listed, nil
}

// DayItems is a day with its items in order.
type DayItems struct {
	Date  string `json:"date"`
	Items []any  `json:"items"`
}

func same(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// days lays out days with their items: the classes by start, each deadline right after the class it is handed in
// at, the rest of the day's deadlines last. It sets the deadlines' at_class and the classes' due.
func days(classes []*ClassItem, deadlineItems []*DeadlineItem, dates []string) []DayItems {
	type day struct {
		classes   []*ClassItem
		deadlines []*DeadlineItem
	}
	byDay := map[string]*day{}
	for _, date := range dates {
		byDay[date] = &day{}
	}
	for _, item := range classes {
		if found := byDay[item.Start[:10]]; found != nil {
			found.classes = append(found.classes, item)
		}
	}
	for _, dl := range deadlineItems {
		found := byDay[dl.Due]
		if found == nil {
			continue
		}
		var candidates []*ClassItem
		for _, c := range found.classes {
			if same(c.Course, dl.Course) && dl.Category == "study" && !c.Cancelled && !c.AllDay &&
				!slices.Contains(schedule.NotClasses, c.Type) {
				candidates = append(candidates, c)
			}
		}
		var at *ClassItem
		for _, c := range candidates {
			if slices.Contains(schedule.Seminars, c.Type) {
				at = c
				break
			}
		}
		if at == nil && len(candidates) > 0 {
			at = candidates[0]
		}
		if at != nil {
			dl.AtClass = &AtClass{UID: at.UID, Start: at.Start, Title: at.Title}
			at.Due = append(at.Due, DueRef{UID: dl.UID, Title: dl.Title})
		}
		found.deadlines = append(found.deadlines, dl)
	}
	out := make([]DayItems, 0, len(dates))
	for _, date := range dates {
		found := byDay[date]
		items := []any{}
		for _, c := range found.classes {
			items = append(items, c)
			for _, dl := range found.deadlines {
				if dl.AtClass != nil && dl.AtClass.UID == c.UID {
					items = append(items, dl)
				}
			}
		}
		for _, dl := range found.deadlines {
			if dl.AtClass == nil {
				items = append(items, dl)
			}
		}
		out = append(out, DayItems{Date: date, Items: items})
	}
	return out
}

// --- screens ---------------------------------------------------------------------------------------------------

func (d *Data) classItems(listed []keyed, now time.Time) []*ClassItem {
	out := make([]*ClassItem, 0, len(listed))
	for _, k := range listed {
		out = append(out, d.ClassItem(k.entry, k.key, now))
	}
	return out
}

func (d *Data) deadlineItems(list []todo.Deadline, today string) ([]*DeadlineItem, error) {
	out := make([]*DeadlineItem, 0, len(list))
	for _, dl := range list {
		item, err := d.DeadlineItem(dl, today)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// Feed is the main screen: the class going on or next today, the overdue deadlines, then every day ahead that has
// a class (within FeedDays) or a deadline (however far). course "" is all of them.
func (d *Data) Feed(course string) (httpx.Object, error) {
	now := d.Now()
	today := dateOf(now)
	start := midnight(today)
	listed, err := d.entries(start, start.AddDate(0, 0, FeedDays), course)
	if err != nil {
		return nil, err
	}
	classes := d.classItems(listed, now)
	list, err := d.deadlines(course)
	if err != nil {
		return nil, err
	}
	all, err := d.deadlineItems(list, today)
	if err != nil {
		return nil, err
	}
	overdue, ahead := []*DeadlineItem{}, []*DeadlineItem{}
	for _, item := range all {
		if item.Overdue {
			overdue = append(overdue, item)
		} else {
			ahead = append(ahead, item)
		}
	}
	dateSet := map[string]bool{today: true}
	for _, c := range classes {
		dateSet[c.Start[:10]] = true
	}
	for _, item := range ahead {
		dateSet[item.Due] = true
	}
	dates := make([]string, 0, len(dateSet))
	for date := range dateSet {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	var current *ClassItem
	for _, c := range classes {
		if !c.Cancelled && !c.AllDay && c.Start[:10] == today && c.End > isoLocal(now) {
			current = c
			break
		}
	}
	courses, err := d.Courses()
	if err != nil {
		return nil, err
	}
	laid := days(classes, ahead, dates)
	return httpx.Object{{Key: "now", Value: isoMinutes(now)}, {Key: "today", Value: today},
		{Key: "course", Value: nullable(course)}, {Key: "courses", Value: courses}, {Key: "current", Value: current},
		{Key: "overdue", Value: overdue}, {Key: "days", Value: laid}}, nil
}

// Week is seven days from the Monday of a day, past ones too, with their classes and deadlines.
func (d *Data) Week(day time.Time, course string) (httpx.Object, error) {
	monday := midnight(day.Format(clock.DateLayout))
	monday = monday.AddDate(0, 0, -((int(monday.Weekday()) + 6) % 7))
	now := d.Now()
	listed, err := d.entries(monday, monday.AddDate(0, 0, 7), course)
	if err != nil {
		return nil, err
	}
	classes := d.classItems(listed, now)
	list, err := d.deadlines(course)
	if err != nil {
		return nil, err
	}
	items, err := d.deadlineItems(list, dateOf(now))
	if err != nil {
		return nil, err
	}
	dates := make([]string, 7)
	for i := range dates {
		dates[i] = monday.AddDate(0, 0, i).Format(clock.DateLayout)
	}
	courses, err := d.Courses()
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "now", Value: isoMinutes(now)}, {Key: "today", Value: dateOf(now)},
		{Key: "start", Value: dates[0]}, {Key: "course", Value: nullable(course)}, {Key: "courses", Value: courses},
		{Key: "days", Value: days(classes, items, dates)}}, nil
}

// Quote is the message an item came from, while it is kept (14 days, D29); nil for no reference.
func (d *Data) Quote(ref *[2]int64, source string) (any, error) {
	if ref == nil {
		return nil, nil
	}
	chatID, msgID := ref[0], ref[1]
	if chatID == inbox.Mailbox {
		source = "mail"
	}
	stored, err := d.Schedule.Inbox.Message(chatID, msgID, source)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return httpx.Object{{Key: "chat", Value: chatID}, {Key: "msg", Value: msgID}, {Key: "text", Value: nil}}, nil
	}
	text := stored.Text
	if pytext.Len(text) > QuoteChars {
		text = strings.TrimRightFunc(pytext.Cut(text, QuoteChars), pytext.IsSpace) + "…"
	}
	return httpx.Object{{Key: "chat", Value: chatID}, {Key: "msg", Value: msgID},
		{Key: "sender", Value: stored.SenderName}, {Key: "at", Value: stored.At},
		{Key: "subject", Value: stored.Subject}, {Key: "text", Value: text}}, nil
}

// object is an item as an ordered object, to add the fields of a card.
func (d *Data) object(item any) httpx.Object {
	o, _ := httpx.Localize(item, d.lang()).(httpx.Object)
	return o
}

// find is a deadline of the merged list, or an own one, by its uid; nil for none.
func (d *Data) find(uid string) (*todo.Deadline, error) {
	found, err := d.Merged.Find(uid)
	if err != nil || found != nil {
		return found, err
	}
	own, err := d.Merged.Todo.Own()
	if err != nil {
		return nil, err
	}
	for _, o := range own {
		if o.Key == uid {
			return &o, nil
		}
	}
	return nil, nil
}

func (d *Data) movedFrom(id int64) (*deadlines.Deadline, error) {
	var older int64
	err := d.Store.DB.QueryRow("select id from deadlines where moved_by = ?", id).Scan(&older)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d.Merged.Deadlines.Get(older)
}

// DeadlineCard is a deadline with its course, where it is handed in, the message it came from and its history; nil
// for none.
func (d *Data) DeadlineCard(uid string) (httpx.Object, error) {
	dl, err := d.find(uid)
	if err != nil || dl == nil {
		return nil, err
	}
	now := d.Now()
	today := dateOf(now)
	item, err := d.DeadlineItem(*dl, today)
	if err != nil {
		return nil, err
	}
	day := midnight(item.Due)
	listed, err := d.entries(day, day.AddDate(0, 0, 1), deref(item.Course))
	if err != nil {
		return nil, err
	}
	days(d.classItems(listed, now), []*DeadlineItem{item}, []string{item.Due})
	row, err := d.row(*dl)
	if err != nil {
		return nil, err
	}
	history := []httpx.Object{}
	var quote, link any
	if row != nil {
		rows := []*deadlines.Deadline{row}
		for at := row; ; {
			older, err := d.movedFrom(at.ID)
			if err != nil {
				return nil, err
			}
			if older == nil {
				break
			}
			rows = append([]*deadlines.Deadline{older}, rows...)
			at = older
		}
		toldBy := func(r *deadlines.Deadline) (any, error) {
			told, err := d.Schedule.ToldBy(schedule.OfDeadline(r))
			return d.told(told), err
		}
		from, err := toldBy(rows[0])
		if err != nil {
			return nil, err
		}
		history = append(history, httpx.Object{{Key: "at", Value: rows[0].CreatedAt}, {Key: "what", Value: "found"},
			{Key: "due", Value: rows[0].Due}, {Key: "from", Value: from}})
		for i := 1; i < len(rows); i++ {
			from, err := toldBy(rows[i])
			if err != nil {
				return nil, err
			}
			history = append(history, httpx.Object{{Key: "at", Value: rows[i].CreatedAt}, {Key: "what", Value: "moved"},
				{Key: "was", Value: rows[i-1].Due}, {Key: "due", Value: rows[i].Due}, {Key: "from", Value: from}})
		}
		if (row.Source == "tg" || row.Source == "mail") && row.ChatID != nil && len(row.MsgIDs) > 0 {
			if quote, err = d.Quote(&[2]int64{*row.ChatID, row.MsgIDs[0]}, row.Source); err != nil {
				return nil, err
			}
			found, err := d.Schedule.Link(schedule.OfDeadline(row))
			if err != nil {
				return nil, err
			}
			link = nullable(found)
		}
	} else if dl.Source == "hseapp" {
		history = append(history, httpx.Object{{Key: "at", Value: nil}, {Key: "what", Value: "hseapp"},
			{Key: "due", Value: item.Due}, {Key: "from", Value: "HSE App X"}})
	} else if dl.Source == "ha" {
		history = append(history, httpx.Object{{Key: "at", Value: nil}, {Key: "what", Value: "own"},
			{Key: "due", Value: item.Due}, {Key: "from", Value: nil}})
	}
	twins, err := d.twins(*dl, today)
	if err != nil {
		return nil, err
	}
	return append(d.object(item), httpx.Field{Key: "quote", Value: quote}, httpx.Field{Key: "link", Value: link},
		httpx.Field{Key: "history", Value: history}, httpx.Field{Key: "own", Value: dl.Source == "ha"},
		httpx.Field{Key: "twins", Value: twins}), nil
}

func dayDistance(a, b time.Time) int {
	n := int(a.Sub(b).Hours() / 24)
	if n < 0 {
		return -n
	}
	return n
}

// twins are what «It is a duplicate…» offers: the other deadlines of the course (of any, for one without a course)
// within two weeks of it, the nearest date first.
func (d *Data) twins(dl todo.Deadline, today string) ([]*DeadlineItem, error) {
	key, err := d.DeadlineKey(dl)
	if err != nil {
		return nil, err
	}
	visible, err := d.Merged.Visible()
	if err != nil {
		return nil, err
	}
	uid := todo.UID(dl)
	found := []todo.Deadline{}
	for _, x := range visible {
		if todo.UID(x) == uid || dayDistance(x.Due, dl.Due) > TwinDays {
			continue
		}
		if key != "" {
			other, err := d.DeadlineKey(x)
			if err != nil {
				return nil, err
			}
			if other != key {
				continue
			}
		}
		found = append(found, x)
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := dayDistance(found[i].Due, dl.Due), dayDistance(found[j].Due, dl.Due)
		if a != b {
			return a < b
		}
		return found[i].Due.Before(found[j].Due)
	})
	if len(found) > Twins {
		found = found[:Twins]
	}
	return d.deadlineItems(found, today)
}

func (d *Data) entry(uid string) (*schedule.Entry, error) {
	all, err := d.Schedule.Entries()
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		if e.UID == uid {
			return &e, nil
		}
	}
	return nil, nil
}

// ClassCard is a class or an event with its course, the deadlines handed in at it, the message that changed it, and
// how much material the library has for it; nil for none.
func (d *Data) ClassCard(uid string) (httpx.Object, error) {
	events, err := d.eventKeys()
	if err != nil {
		return nil, err
	}
	entry, err := d.entry(uid)
	if err != nil || entry == nil {
		return nil, err
	}
	now := d.Now()
	key := d.EntryKey(*entry, events)
	item := d.ClassItem(*entry, key, now)
	day := item.Start[:10]
	dues := []*DeadlineItem{}
	if key != "" {
		list, err := d.deadlines(key)
		if err != nil {
			return nil, err
		}
		for _, dl := range list {
			if todo.DueText(dl.Due) == day {
				found, err := d.DeadlineItem(dl, dateOf(now))
				if err != nil {
					return nil, err
				}
				dues = append(dues, found)
			}
		}
	}
	days([]*ClassItem{item}, dues, []string{day})
	source := "tg"
	if entry.Message != nil && entry.Message[0] == inbox.Mailbox {
		source = "mail"
	}
	materials := 0
	if d.Library != nil {
		items, err := d.Library.Items(library.Query{ClassUID: &entry.UID})
		if err != nil {
			return nil, err
		}
		materials = len(items)
	}
	title, err := d.Title(key)
	if err != nil {
		return nil, err
	}
	quote, err := d.Quote(entry.Message, source)
	if err != nil {
		return nil, err
	}
	fix, err := d.Fix(*entry)
	if err != nil {
		return nil, err
	}
	return append(d.object(item), httpx.Field{Key: "course_title", Value: nullable(title)},
		httpx.Field{Key: "source", Value: entry.Source}, httpx.Field{Key: "origin", Value: nullable(entry.Origin)},
		httpx.Field{Key: "link", Value: nullable(entry.Link)}, httpx.Field{Key: "quote", Value: quote},
		httpx.Field{Key: "materials", Value: materials}, httpx.Field{Key: "fix", Value: fix}), nil
}

// --- corrections (step 16.9) ---------------------------------------------------------------------------------

// span is start and end as the tools take them: Moscow minutes, or a date and no end for a whole day.
func span(start, end time.Time, wholeDay bool) httpx.Object {
	if wholeDay {
		return httpx.Object{{Key: "start", Value: dateOf(start)}, {Key: "end", Value: nil}}
	}
	return httpx.Object{{Key: "start", Value: isoMinutes(start)}, {Key: "end", Value: isoMinutes(end)}}
}

// Fix is what stands behind an entry for the owner to change, with every field as it is now: correct_class and
// edit_event reset a field they are not given, so the app sends them all and changes only what was touched. A
// class: RUZ's own time, the owner's correction (`own`, its event id) and the chat's one (`chat`, applied unless
// hidden). An event: its id, whether it is the owner's own or an own copy of a chat's one. Nil for neither.
func (d *Data) Fix(entry schedule.Entry) (any, error) {
	evs := d.Schedule.Events
	if entry.OfClass {
		item, err := d.Schedule.Ruz.ByUID(entry.UID)
		if err != nil || item == nil {
			return nil, err
		}
		own, err := evs.OwnCorrection(entry.UID)
		if err != nil {
			return nil, err
		}
		chat, err := evs.Correction(entry.UID)
		if err != nil {
			return nil, err
		}
		behind := own
		if behind == nil && chat != nil && schedule.Applies(item, chat) {
			behind = chat
		}
		room := item.Room
		if behind != nil {
			room = behind.Room
		}
		kind := item.Kind
		if own != nil {
			kind = own.Kind
		}
		fields := append(span(entry.Start, entry.End, entry.AllDay), httpx.Field{Key: "room", Value: room},
			httpx.Field{Key: "cancelled", Value: entry.Cancelled}, httpx.Field{Key: "kind", Value: kind},
			httpx.Field{Key: "required", Value: entry.Required})
		if own != nil && own.Title != "" {
			fields = append(fields, httpx.Field{Key: "title", Value: own.Title})
		}
		start, err := schedule.Local(item.Start)
		if err != nil {
			return nil, err
		}
		end, err := schedule.Local(item.End)
		if err != nil {
			return nil, err
		}
		var ownID, chatField any
		if own != nil {
			ownID = own.ID
		}
		if chat != nil {
			told, err := d.Schedule.ToldBy(schedule.OfEvent(chat))
			if err != nil {
				return nil, err
			}
			chatField = httpx.Object{{Key: "id", Value: chat.ID}, {Key: "hidden", Value: chat.Hidden},
				{Key: "from", Value: d.told(told)}}
		}
		return httpx.Object{{Key: "of", Value: "class"}, {Key: "fields", Value: fields}, {Key: "own", Value: ownID},
			{Key: "chat", Value: chatField},
			{Key: "ruz", Value: append(span(start, end, false), httpx.Field{Key: "room", Value: item.Room},
				httpx.Field{Key: "cancelled", Value: item.Cancelled})}}, nil
	}
	event, err := evs.ByUID(entry.UID)
	if err != nil || event == nil {
		return nil, err
	}
	fields := append(httpx.Object{{Key: "title", Value: event.Title}, {Key: "kind", Value: event.Kind}},
		span(entry.Start, entry.End, entry.AllDay)...)
	fields = append(fields, httpx.Field{Key: "room", Value: event.Room},
		httpx.Field{Key: "cancelled", Value: event.Cancelled})
	return httpx.Object{{Key: "of", Value: "event"}, {Key: "id", Value: event.ID},
		{Key: "own", Value: event.Source == events.Own}, {Key: "copy", Value: deref(event.CopyOf) != ""},
		{Key: "required", Value: event.Required}, {Key: "fields", Value: fields}}, nil
}

// Hidden is what the owner hid, to bring back: the deadlines, the newest due date first, and the events from today
// on (an event an own copy stands in for is not listed: «As it was» on the copy brings it back).
func (d *Data) Hidden() (httpx.Object, error) {
	now := d.Now()
	reasons := map[string]i18n.Phrase{"done": i18n.P("done", "сдан"), "not_mine": i18n.P("not mine", "не мой"),
		"duplicate": i18n.P("a duplicate", "дубль")}
	rows, err := d.Merged.Todo.HiddenRows()
	if err != nil {
		return nil, err
	}
	listed := []httpx.Object{}
	for _, row := range rows {
		var course, reason, from any
		if row.Course != nil && *row.Course != "" {
			key, err := d.Key(*row.Course)
			if err != nil {
				return nil, err
			}
			course = nullable(key)
		}
		if phrase, ok := reasons[deref(row.Reason)]; ok {
			reason = d.told(phrase)
		}
		if phrase, ok := SourceNames[row.Source]; ok {
			from = d.told(phrase)
		}
		listed = append(listed, httpx.Object{{Key: "uid", Value: row.UID}, {Key: "title", Value: row.Title},
			{Key: "due", Value: row.Due}, {Key: "course", Value: course}, {Key: "reason", Value: reason},
			{Key: "from", Value: from}})
	}
	agenda, err := d.Schedule.Agenda(midnight(dateOf(now)), 3650)
	if err != nil {
		return nil, err
	}
	hidden := httpx.Localize(agenda.Hidden, d.lang())
	if hidden == nil {
		hidden = []any{}
	}
	return httpx.Object{{Key: "deadlines", Value: listed}, {Key: "events", Value: hidden}}, nil
}

// --- news (step 16.7) ------------------------------------------------------------------------------------------

func (d *Data) newsItem(item *news.Item) (httpx.Object, error) {
	in := d.Schedule.Inbox
	var where httpx.Object
	if item.Source == "mail" {
		subject, err := in.Subject(item.MsgIDs)
		if err != nil {
			return nil, err
		}
		where = httpx.Object{{Key: "kind", Value: "mail"}, {Key: "name", Value: subject}}
	} else {
		chat, err := in.Chat(item.ChatID)
		if err != nil {
			return nil, err
		}
		where = httpx.Object{{Key: "kind", Value: "chat"}, {Key: "name", Value: nil}}
		if chat != nil {
			where = httpx.Object{{Key: "kind", Value: chat.Kind}, {Key: "name", Value: chat.Title}}
		}
	}
	return httpx.Object{{Key: "id", Value: item.ID}, {Key: "full", Value: item.Full}, {Key: "short", Value: item.Short},
		{Key: "importance", Value: item.Importance}, {Key: "study", Value: item.Study},
		{Key: "posted_at", Value: item.PostedAt}, {Key: "from", Value: where}}, nil
}

func (d *Data) ttl() int {
	if d.TTLHours == nil {
		return 24
	}
	return d.TTLHours()
}

// NewsList is the live news in rank order, as Tablo has them.
func (d *Data) NewsList() (httpx.Object, error) {
	items := []httpx.Object{}
	if d.News != nil {
		live, err := d.News.Live(d.Schedule.Now(), d.ttl())
		if err != nil {
			return nil, err
		}
		for _, item := range live {
			o, err := d.newsItem(item)
			if err != nil {
				return nil, err
			}
			items = append(items, o)
		}
	}
	return httpx.Object{{Key: "items", Value: items}, {Key: "ttl_hours", Value: d.ttl()}}, nil
}

// NewsCard is an item with the message it came from, while it is kept, and its link where the chat has one; nil
// for none or a hidden one.
func (d *Data) NewsCard(newsID int64) (httpx.Object, error) {
	if d.News == nil {
		return nil, nil
	}
	item, err := d.News.Get(newsID)
	if err != nil || item == nil || item.Hidden {
		return nil, err
	}
	var quote, link any
	if len(item.MsgIDs) > 0 {
		if quote, err = d.Quote(&[2]int64{item.ChatID, item.MsgIDs[0]}, item.Source); err != nil {
			return nil, err
		}
		if item.Source != "mail" {
			chat, err := d.Schedule.Inbox.Chat(item.ChatID)
			if err != nil {
				return nil, err
			}
			if chat != nil {
				link = nullable(inbox.MessageLink(chat, item.MsgIDs[0]))
			}
		}
	}
	o, err := d.newsItem(item)
	if err != nil {
		return nil, err
	}
	return append(o, httpx.Field{Key: "quote", Value: quote}, httpx.Field{Key: "link", Value: link},
		httpx.Field{Key: "live", Value: item.ReplacedBy == nil && item.GoneAt == nil}), nil
}

// names are the timetable's names of a course key, sorted.
func (d *Data) names(key string) ([]string, error) {
	classes, err := d.Schedule.Ruz.Classes("", "9999")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, item := range classes {
		found, err := d.Key(item.Subject)
		if err != nil {
			return nil, err
		}
		if found == key {
			set[item.Subject] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Course is a course's lens: its title, next class, grades and shelves (knowledge base, sheets, raw material); nil
// for no such course.
func (d *Data) Course(key string) (httpx.Object, error) {
	names, err := d.names(key)
	if err != nil {
		return nil, err
	}
	known := len(names) > 0
	if !known {
		courses, err := d.Courses()
		if err != nil {
			return nil, err
		}
		for _, c := range courses {
			known = known || c.Key == key
		}
	}
	if !known {
		list, err := d.deadlines(key)
		if err != nil {
			return nil, err
		}
		known = len(list) > 0 && key != ""
	}
	if !known {
		return nil, nil
	}
	now := d.Now()
	listed, err := d.entries(now, now.AddDate(0, 0, AheadCourseDays), key)
	if err != nil {
		return nil, err
	}
	var next *ClassItem
	for _, k := range listed {
		if !k.entry.Cancelled {
			next = d.ClassItem(k.entry, k.key, now)
			break
		}
	}
	if next != nil {
		day := next.Start[:10]
		list, err := d.deadlines(key)
		if err != nil {
			return nil, err
		}
		dues := []*DeadlineItem{}
		for _, dl := range list {
			if todo.DueText(dl.Due) == day {
				item, err := d.DeadlineItem(dl, dateOf(now))
				if err != nil {
					return nil, err
				}
				dues = append(dues, item)
			}
		}
		days([]*ClassItem{next}, dues, []string{day})
	}
	title, err := d.Title(key)
	if err != nil {
		return nil, err
	}
	gradesView, err := d.courseGrades(key, names)
	if err != nil {
		return nil, err
	}
	documents, err := d.documents(names)
	if err != nil {
		return nil, err
	}
	materials, err := d.materials(names)
	if err != nil {
		return nil, err
	}
	sheets, err := d.sheets(key)
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "key", Value: key}, {Key: "title", Value: title}, {Key: "next", Value: next},
		{Key: "grades", Value: gradesView}, {Key: "documents", Value: documents}, {Key: "materials", Value: materials},
		{Key: "sheets", Value: sheets}}, nil
}

func (d *Data) documents(names []string) ([]httpx.Object, error) {
	docs := []httpx.Object{}
	if d.Library == nil || len(names) == 0 {
		return docs, nil
	}
	found, err := d.Library.Documents(&names[0])
	if err != nil {
		return nil, err
	}
	type doc struct {
		object httpx.Object
		day    string
		kind   string
	}
	listed := []doc{}
	for _, summary := range found {
		if summary.Version == nil || *summary.Version == 0 {
			continue
		}
		part := "pdf"
		if library.TextOnly(summary.Kind) {
			part = "source"
		}
		full, err := d.Library.GetDocument(summary.ID)
		if err != nil {
			return nil, err
		}
		versions := []httpx.Object{}
		var newest any
		if full != nil {
			for _, row := range full.Versions {
				version := &library.Version{VersionRow: row, Course: summary.Course, Kind: summary.Kind,
					Title: summary.Title, ClassUID: summary.ClassUID}
				var url any
				if d.Links != nil && d.Library.PartPath(version, part) != "" {
					url = d.Links.Version(version, part, links.GetTTL)
				}
				if row.Version != nil && *row.Version == *summary.Version && newest == nil {
					newest = url
				}
				versions = append(versions, httpx.Object{{Key: "version", Value: row.Version},
					{Key: "published_at", Value: row.PublishedAt}, {Key: "note", Value: row.Note}, {Key: "url", Value: url}})
			}
		}
		listed = append(listed, doc{httpx.Object{{Key: "id", Value: summary.ID}, {Key: "kind", Value: summary.Kind},
			{Key: "title", Value: summary.Title}, {Key: "day", Value: summary.Day}, {Key: "url", Value: newest},
			{Key: "versions", Value: versions}}, deref(summary.Day), summary.Kind})
	}
	sort.SliceStable(listed, func(i, j int) bool { return listed[i].day > listed[j].day })
	order := func(kind string) int {
		switch kind {
		case "book":
			return 0
		case "cheatsheet":
			return 1
		}
		return 2
	}
	sort.SliceStable(listed, func(i, j int) bool { return order(listed[i].kind) < order(listed[j].kind) })
	for _, item := range listed {
		docs = append(docs, item.object)
	}
	return docs, nil
}

func (d *Data) materials(names []string) (httpx.Object, error) {
	count, fresh := int64(0), int64(0)
	if d.Library != nil && len(names) > 0 {
		course, err := d.Library.Course(names[0])
		if err != nil {
			return nil, err
		}
		courses, err := d.Library.Courses()
		if err != nil {
			return nil, err
		}
		for _, c := range courses {
			if c.Course == course {
				count, fresh = c.Items, c.New
				break
			}
		}
	}
	return httpx.Object{{Key: "count", Value: count}, {Key: "new", Value: fresh}}, nil
}

func (d *Data) sheets(key string) (httpx.Object, error) {
	rows, err := d.Store.DB.Query("select course from sheets where previous is null and status != 'failed'")
	if err != nil {
		return nil, err
	}
	var courses []string
	for rows.Next() {
		var course sql.NullString
		if err := rows.Scan(&course); err != nil {
			rows.Close()
			return nil, err
		}
		courses = append(courses, course.String)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	count := 0
	for _, course := range courses {
		found, err := d.Key(course)
		if err != nil {
			return nil, err
		}
		if found == key {
			count++
		}
	}
	return httpx.Object{{Key: "count", Value: count}}, nil
}
