// Package settings holds the general and chat settings (§5.1, §10): kept in SQLite, edited in the web app. A setting
// never set has its default.
//
// General settings arrive with the stages that use them. Chat settings are all here from the start, so the web app
// can set a chat up once; the twin uses `enabled`, the batches `study`, `filter`, `people`, `urgent`, `batch_hours`
// and `images` and `note`, the sheets `pdf_content`. A saved setting no longer listed here (the summaries'
// `summary_time` and `summary`, removed with them, D55) stays in its table and is never read.
//
// Values are kept as Python's json.dumps(ensure_ascii=False) writes them: floats as Float ("1.0"), objects as structs
// with their keys in Python's order. Get decodes a stored value into its setting's type (Float, int, string, bool,
// []string, []int64, []GradeSheet, map[string]Price, *int), so a default and a saved value look the same.
//
// Numbers: encoding/json decodes every number as float64, so a whole float64 counts as an int (Python would refuse
// 3.0 for a setting of hours); a decoder with UseNumber keeps them apart, and json.Number "3.0" is refused.
package settings

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// spec is a setting's default and check; the check returns the value to keep or an *Error.
type spec struct {
	def   any
	check func(any) (any, error)
	typ   reflect.Type
}

func mk[T any](def T, check func(any) (T, error)) spec {
	return spec{def: def, typ: reflect.TypeFor[T](),
		check: func(v any) (any, error) {
			out, err := check(v)
			if err != nil {
				return nil, err
			}
			return out, nil
		}}
}

// Names are the general settings, in their order.
var Names = []string{
	"llm_daily_limit_usd", "llm_prices", "batch_hours", "news_ttl_hours", "hseapp_enabled", "pdf_pages", "pdf_tokens",
	"images_daily", "deadline_reminders", "reminder_eve", "reminder_day", "grades_student", "grades_sheets",
	"grades_window_days", "language", "about_me", "wiki_pages", "push_kinds", "push_quiet",
}

var specs = map[string]spec{
	"llm_daily_limit_usd": mk(Float(0.50), positiveMoney), // D27
	"llm_prices": mk(map[string]Price{"claude-haiku-5-5": {Input: 0.10, Output: 0.50, LongInput: 0.50,
		LongOutput: 2.50}}, prices),
	"batch_hours":    mk(3, generalBatchHours), // D19
	"news_ttl_hours": mk(24, hours),            // a news item lives this long; older non-study messages are skipped
	// Poll HSE App X (§5.2). One client per token chain: the refresh token rotates, so Tablo's own must be off.
	"hseapp_enabled": mk(true, flag),
	// Reading a PDF whole (§6.3, D14): at most so many pages and tokens of a file, what comes first.
	"pdf_pages":  mk(10, whole(1, 50, "a number of pages")),
	"pdf_tokens": mk(30_000, whole(1_000, 200_000, "a number of tokens")),
	// Images read a Moscow day in chats with «Картинки» (D12); inside the LLM limit, off above its 80 % (§7).
	"images_daily": mk(30, whole(0, 500, "a number of images")),
	// Deadline reminders, pushed (D47): on the eve, what is due tomorrow; in the morning, what is due today.
	"deadline_reminders": mk(true, flag),
	"reminder_eve":       mk("20:00", clockTime),
	"reminder_day":       mk("09:00", clockTime),
	// Grades from teachers' Google Sheets (§6.7, Tablo's §3.2): the own row is found by the name.
	"grades_student":     mk("", student),
	"grades_sheets":      mk([]GradeSheet{}, gradeSheets),
	"grades_window_days": mk(7, whole(1, 60, "a number of days")), // a grade is new this long: the API for Tablo
	// What the owner reads (D41): the app, the bot's answers and what the models write for them (a sheet's summary).
	"language": mk("en", language),
	// What the student tells the batches about themselves (D52): group, subgroup, English group,
	// electives, so that what is meant for others does not count.
	"about_me": mk("", text(AboutMeChars)),
	// Course pages of the faculty's wiki (D60): their new sheets and homework come as notices in the mailbox.
	"wiki_pages": mk([]string{}, wikiPages),
	// The web app's notifications (§8.6, D65): which kinds are pushed, and the hours they come without a sound.
	"push_kinds": mk(slices.Clone(PushKinds), pushKinds),
	"push_quiet": mk("", quietHours),
}

// ChatNames are the chat settings, in their order.
var ChatNames = []string{"enabled", "study", "filter", "people", "images", "pdf_content", "urgent", "batch_hours",
	"note"}

var chatSpecs = map[string]spec{
	"enabled":     mk(false, flag), // a new chat is off (D11)
	"study":       mk(false, flag),
	"filter":      mk("all", filter),
	"people":      mk([]int64{}, people),
	"images":      mk(false, flag),
	"pdf_content": mk(false, flag),
	"urgent":      mk(false, flag),
	"batch_hours": mk[*int](nil, chatBatchHours),
	"note":        mk("", text(ChatNoteChars)), // for the batches: its course, who is who (D52)
}

// Settings renamed or gone: the new name takes the old row's value once (D69); a gone one's row is dropped.
var (
	renamed = [][2]string{{"miniapp_language", "language"}}
	gone    = []string{"forum_language"}
)

func generalSpec(name string) (spec, error) {
	s, ok := specs[name]
	if !ok {
		return spec{}, errorf("unknown setting %s", repr(name))
	}
	return s, nil
}

func chatSpec(name string) (spec, error) {
	s, ok := chatSpecs[name]
	if !ok {
		return spec{}, errorf("unknown chat setting %s", repr(name))
	}
	return s, nil
}

// Check is a general setting's check without saving (SPECS[name][1](value)): the value to keep, or an *Error.
func Check(name string, value any) (any, error) {
	s, err := generalSpec(name)
	if err != nil {
		return nil, err
	}
	return s.check(value)
}

// ChatCheck is a chat setting's check without saving.
func ChatCheck(name string, value any) (any, error) {
	s, err := chatSpec(name)
	if err != nil {
		return nil, err
	}
	return s.check(value)
}

// Default is a fresh copy of a general setting's default.
func Default(name string) (any, error) {
	s, err := generalSpec(name)
	if err != nil {
		return nil, err
	}
	return s.fresh()
}

// encode is the stored text of a value: json.dumps(value, ensure_ascii=False).
func encode(value any) (string, error) { return pyjson.Dumps(value) }

var (
	intType    = reflect.TypeFor[int]()
	intPtrType = reflect.TypeFor[*int]()
)

// decode reads a stored text into the setting's type. A whole number Python wrote as a float ("3.0": the batch hours
// take it) reads as an int.
func (s spec) decode(text string) (any, error) {
	if s.typ == intType || s.typ == intPtrType {
		var v any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			return nil, err
		}
		if v == nil && s.typ == intPtrType {
			return (*int)(nil), nil
		}
		n, ok := integer(v)
		if !ok {
			return nil, fmt.Errorf("stored setting %s is not a whole number", text)
		}
		if s.typ == intPtrType {
			i := int(n)
			return &i, nil
		}
		return int(n), nil
	}
	out := reflect.New(s.typ)
	if err := json.Unmarshal([]byte(text), out.Interface()); err != nil {
		return nil, err
	}
	return out.Elem().Interface(), nil
}

// fresh is a copy of the default no caller shares.
func (s spec) fresh() (any, error) {
	text, err := encode(s.def)
	if err != nil {
		return nil, err
	}
	return s.decode(text)
}

// Value is one setting of All.
type Value struct {
	Name  string
	Value any
}

// Values are settings in their order; JSON writes them as one object in that order.
type Values []Value

// Get is the value of a name, nil when it is not there.
func (v Values) Get(name string) any {
	for _, item := range v {
		if item.Name == name {
			return item.Value
		}
	}
	return nil
}

// Map is the values by name.
func (v Values) Map() map[string]any {
	m := make(map[string]any, len(v))
	for _, item := range v {
		m[item.Name] = item.Value
	}
	return m
}

// MarshalJSON writes the values as an object, in their order.
func (v Values) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, item := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := pyjson.CompactASCII(item.Name)
		if err != nil {
			return nil, err
		}
		value, err := pyjson.Compact(item.Value) // no HTML escapes: links keep their &
		if err != nil {
			return nil, err
		}
		b.WriteString(key)
		b.WriteByte(':')
		b.WriteString(value)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// Settings are the general settings.
type Settings struct {
	db *sql.DB
}

// New opens the general settings, moving renamed ones and dropping gone ones.
func New(s *store.Store) (*Settings, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, r := range renamed {
		old, now := r[0], r[1]
		if _, err := tx.Exec("update settings set name = ? where name = ? and not exists "+
			"(select 1 from settings where name = ?)", now, old, now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("delete from settings where name = ?", old); err != nil {
			return nil, err
		}
	}
	for _, name := range gone {
		if _, err := tx.Exec("delete from settings where name = ?", name); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Settings{db: s.DB}, nil
}

// Get is a setting's value, its default when never set.
func (s *Settings) Get(name string) (any, error) {
	sp, err := generalSpec(name)
	if err != nil {
		return nil, err
	}
	var text string
	err = s.db.QueryRow("select value from settings where name = ?", name).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return sp.fresh()
	}
	if err != nil {
		return nil, err
	}
	return sp.decode(text)
}

// Set checks and saves a value; it returns the value kept.
func (s *Settings) Set(name string, value any) (any, error) {
	sp, err := generalSpec(name)
	if err != nil {
		return nil, err
	}
	value, err = sp.check(value)
	if err != nil {
		return nil, err
	}
	text, err := encode(value)
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec("insert into settings(name, value) values(?, ?) "+
		"on conflict(name) do update set value = excluded.value", name, text)
	if err != nil {
		return nil, err
	}
	return value, nil
}

// Reset drops a saved value: the default comes back.
func (s *Settings) Reset(name string) error {
	if _, err := generalSpec(name); err != nil {
		return err
	}
	_, err := s.db.Exec("delete from settings where name = ?", name)
	return err
}

// All is every general setting, in the order of Names.
func (s *Settings) All() (Values, error) {
	values := make(Values, 0, len(Names))
	for _, name := range Names {
		v, err := s.Get(name)
		if err != nil {
			return nil, err
		}
		values = append(values, Value{name, v})
	}
	return values, nil
}

func typed[T any](s *Settings, name string) (T, error) {
	var zero T
	v, err := s.Get(name)
	if err != nil {
		return zero, err
	}
	out, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("setting %s is %T, not %T", name, v, zero)
	}
	return out, nil
}

// Float is a setting of dollars.
func (s *Settings) Float(name string) (float64, error) {
	v, err := typed[Float](s, name)
	return float64(v), err
}

// Int is a setting of a whole number.
func (s *Settings) Int(name string) (int, error) { return typed[int](s, name) }

// String is a setting of text.
func (s *Settings) String(name string) (string, error) { return typed[string](s, name) }

// Bool is a flag.
func (s *Settings) Bool(name string) (bool, error) { return typed[bool](s, name) }

// Strings is a setting of a list of texts (wiki_pages, push_kinds).
func (s *Settings) Strings(name string) ([]string, error) { return typed[[]string](s, name) }

// Prices is llm_prices.
func (s *Settings) Prices() (map[string]Price, error) {
	return typed[map[string]Price](s, "llm_prices")
}

// GradeSheets is grades_sheets.
func (s *Settings) GradeSheets() ([]GradeSheet, error) {
	return typed[[]GradeSheet](s, "grades_sheets")
}

// Chat is the settings of one chat; its fields are in the order of ChatNames.
type Chat struct {
	Enabled    bool    `json:"enabled"`
	Study      bool    `json:"study"`
	Filter     string  `json:"filter"`
	People     []int64 `json:"people"`
	Images     bool    `json:"images"`
	PDFContent bool    `json:"pdf_content"`
	Urgent     bool    `json:"urgent"`
	BatchHours *int    `json:"batch_hours"` // nil: the general N
	Note       string  `json:"note"`
}

// Get is one setting of the chat by its name.
func (c Chat) Get(name string) (any, error) {
	if _, err := chatSpec(name); err != nil {
		return nil, err
	}
	rv := reflect.ValueOf(c)
	for i := range rv.NumField() {
		tag, _, _ := strings.Cut(rv.Type().Field(i).Tag.Get("json"), ",")
		if tag == name {
			return rv.Field(i).Interface(), nil
		}
	}
	return nil, errorf("unknown chat setting %s", repr(name))
}

// ChatDefaults are the settings of a chat never set up.
func ChatDefaults() Chat {
	return Chat{Filter: "all", People: []int64{}}
}

// ChatSettings are the settings of each chat (§5.1), stored per chat and name like the general ones.
type ChatSettings struct {
	db *sql.DB
}

// NewChats opens the chat settings.
func NewChats(s *store.Store) *ChatSettings { return &ChatSettings{db: s.DB} }

// Get is one setting of a chat.
func (c *ChatSettings) Get(chatID int64, name string) (any, error) {
	all, err := c.All(chatID)
	if err != nil {
		return nil, err
	}
	return all.Get(name)
}

// All is every setting of a chat; saved names no longer listed are left out.
func (c *ChatSettings) All(chatID int64) (Chat, error) {
	rows, err := c.db.Query("select name, value from chat_settings where chat_id = ?", chatID)
	if err != nil {
		return Chat{}, err
	}
	defer rows.Close()
	chat := ChatDefaults()
	fields := reflect.ValueOf(&chat).Elem()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return Chat{}, err
		}
		sp, ok := chatSpecs[name]
		if !ok {
			continue
		}
		decoded, err := sp.decode(value)
		if err != nil {
			return Chat{}, fmt.Errorf("chat %d, setting %s: %w", chatID, name, err)
		}
		fields.Field(slices.Index(ChatNames, name)).Set(reflect.ValueOf(decoded))
	}
	if err := rows.Err(); err != nil {
		return Chat{}, err
	}
	if chat.People == nil {
		chat.People = []int64{}
	}
	return chat, nil
}

// Update checks every value first, then saves them together; it returns all settings of the chat. Names are checked
// in sorted order, so the error of several bad values is always the same one.
func (c *ChatSettings) Update(chatID int64, values map[string]any) (Chat, error) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	texts := make([]string, len(names))
	for i, name := range names {
		checked, err := ChatCheck(name, values[name])
		if err != nil {
			return Chat{}, err
		}
		if texts[i], err = encode(checked); err != nil {
			return Chat{}, err
		}
	}
	tx, err := c.db.Begin()
	if err != nil {
		return Chat{}, err
	}
	defer tx.Rollback()
	for i, name := range names {
		if _, err := tx.Exec("insert into chat_settings(chat_id, name, value) values(?, ?, ?) "+
			"on conflict(chat_id, name) do update set value = excluded.value", chatID, name, texts[i]); err != nil {
			return Chat{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Chat{}, err
	}
	return c.All(chatID)
}

// Enabled reports whether the twin reads the chat.
func (c *ChatSettings) Enabled(chatID int64) (bool, error) {
	all, err := c.All(chatID)
	return all.Enabled, err
}

// EnabledIDs are the chats the twin reads.
func (c *ChatSettings) EnabledIDs() (map[int64]bool, error) { return c.FlaggedIDs("enabled") }

// FlaggedIDs are the chats where a flag is on: a flag is off by default, so only saved rows can turn it on.
func (c *ChatSettings) FlaggedIDs(name string) (map[int64]bool, error) {
	if _, err := chatSpec(name); err != nil {
		return nil, err
	}
	rows, err := c.db.Query("select chat_id from chat_settings where name = ? and value = 'true'", name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}
