// Package batches is the batches (§6.1, D19, D30): new messages of a chat go to Haiku together, news, deadlines and
// events come back. The port of processing/batches.py.
//
// When:
//   - on schedule: every enabled chat is parsed at the hours of the day that divide by its N (`batch_hours` of the
//     chat, else the general one), 0, 3, 6… for N = 3. A slot missed while the service was down or the limit was
//     spent is made up at the next hourly tick;
//   - soon, if the chat is "urgent" and a pinned message, a PDF or a message of a listed person arrives. Messages
//     often come in bursts (an album, an announcement in three messages, its correction), so the batch waits until
//     the chat has been quiet for UrgentQuiet: every new message of the chat, urgent or not, stretches the wait, but
//     never beyond UrgentMaxWait after the trigger.
//
// What goes to the model: messages of the chat that wait for a batch (Inbox.Pending), except non-study ones older
// than `news_ttl_hours` (their news would not be live anyway), messages with nothing to read (a sticker alone, a photo
// alone whose image is not read) and, in a chat with the `important` filter, messages that are not important. Those
// are marked parsed without a call. Study messages are never skipped for age: they carry deadlines.
//
// Each message is seen once: `parsed_at` is set when the answer is saved. A failed call leaves the messages in the
// queue. A message edited after its batch is parsed again, and what it gives replaces what its earlier text gave; a
// deleted message withdraws what it gave. Study chats go first, then the mailbox, then the others (D30).
//
// Letters of the university mailbox (collectors/mail, §5.2) are parsed the same way, with the same prompt and schema:
// the mailbox is one more chat (Mailbox), study (every letter is, §6.2), without a filter or urgency, on the general
// schedule while mail is on or letters wait. The prompt says it is the student's mailbox and gives each letter's
// subject; a letter may be twice as long as a chat message (8000 characters, as Tablo gave them). What letters give
// has the source "mail", and its events go to the feeds with X-INFOMASTER-SOURCE mail.
//
// Events (§6.5) are consultations, talks, meetings: things to attend that are not in the timetable. The model sees
// the upcoming chat events (e...) and, in a study chat, the RUZ classes of the next weeks (c...), so it can point a
// move or a cancellation at one of them: a chat event gets a new version with the same UID, a class gets a correction
// (§6.5, D28). A class counts by the later of its RUZ time and its correction's, so a moved class stays listed until
// the move is past. The owner's own events are listed too, so a chat does not repeat them, but the model does not
// move them. A new event that only repeats a class (Schedule.RepeatsClass) is not saved, nor is a move of a class the
// model was not shown. An event whose day is told without its time is an all-day one, unless that day has the same
// event with a time: then it only gives that one its room, and a timed one gives an all-day one its time. The model
// also sees the whole course dictionary (§6.1, §6.2): each course with its title, its abbreviation and every name it
// goes by, so it knows a course under any name («ВвП», «матан»), gives it as the dictionary names it and uses the
// dictionary's abbreviations in labels, titles and news. Course names of the saved deadlines and events go to the
// dictionary; only names it does not know yet cost a call. After every run the Midterms follow the corrections of
// their classes (§4.4).
//
// Sheets (§6.3, D14): a PDF of a chat message is downloaded when its batch comes (collectors/files), and the model
// sees its first page: the text, or for a scan the page itself as a document. The model says which PDFs are sheets,
// with their course, number and due date; they become rows of processing/sheets in the same transaction, and are
// published after the run. A sheet without a date gets the next seminar's, marked assumed in the deadlines the model
// sees: a message that tells the real date moves it.
//
// Images (D12): in a chat with «Картинки» on, the image of a message is downloaded when its batch comes and read by
// Haiku in a call of its own (processing/images), within the daily number of images and while optional tasks run.
// The model of the batch sees what was read on it: what it shows and its text.
//
// Materials (D58, §6.8): in a study chat the model also says which messages carry course material (a photo of the
// board, notes, slides, a recording, a link to one); only there is the field in the schema. A photo, a recording or a
// voice message without text goes to the model there too when the library is on, so it can be told; elsewhere such a
// message is skipped as before. The materials are queued in the batch's transaction and taken into the library after
// the run, outside it (library/feed): a file is downloaded then, never while the messages are being parsed.
//
// Context: before the new messages of a chat the model sees up to ContextMessages earlier, parsed ones of the same
// chat and forum topic, from ContextAge before the first new one, each cut to ContextChars: what a short reply or
// «перенесли на пятницу» refers to. They give no items; a new message that replies to one of them says so instead of
// quoting it. Letters get none: each letter stands on its own.
//
// The student (D52): what they wrote about themselves (`about_me`: group, subgroup, English group, electives) and the
// chat's note (`note`: its course, who is who) go to the prompt, so that what is meant for other groups counts for
// little and a task without a course name gets the chat's. Deadlines passed in the last PassedDeadlines are listed
// too, marked passed, so that an extension after the date moves them instead of adding another.
//
// One connection (PORTING.md): everything saved for a chunk is written in one transaction through repositories bound
// to it; the timetable's class entries a new event is compared with are read through a view of the schedule bound to
// the same transaction.
package batches

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/ruz"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/library/feed"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/deadlines"
	"github.com/IvanIkra/infomaster/go/internal/processing/events"
	"github.com/IvanIkra/infomaster/go/internal/processing/hseapp"
	"github.com/IvanIkra/infomaster/go/internal/processing/images"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/processing/pdf"
	"github.com/IvanIkra/infomaster/go/internal/processing/schedule"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/subjects"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	FailedKey  = "batches.failed" // {"source:chat:msg": failed runs} of messages the model could not answer for
	DoneKey    = "batches.done"
	SkipAfter  = 3 // failed runs before such a message is skipped
	MaxTokens  = 16_000
	ChunkMsgs  = 40     // an item costs up to ≈250 output tokens, and most chat messages give none
	ChunkChars = 24_000 // of message text in one call
	TextLimit  = 4_000  // characters of one message in the prompt
	// MailTextLimit is of one letter: as much as Tablo gave Haiku (§6.2).
	MailTextLimit   = 8_000
	QuoteLimit      = 300                 // characters of a replied message that is not in the batch
	ContextMessages = 6                   // earlier messages of the chat before the new ones, for context only
	ContextChars    = 300                 // characters of each of them
	ContextAge      = 12 * time.Hour      // before the first new message: older talk is another conversation
	PassedDeadlines = 7 * 24 * time.Hour  // passed ones are shown this long: an extension after the date moves them
	FirstPageLimit  = 2_000               // characters of a PDF's first page in the prompt
	ImageLimit      = 2_500               // characters of what was read on an image, in the prompt
	TickDelay       = 5 * time.Second     // past the hour
	RetractWindow   = 15 * 24 * time.Hour // raw messages live 14 days: older items cannot be checked for deletion

	Line      = 28 // characters in a line of Tablo's news column (§2.1)
	FullChars = 2 * Line
	// LabelChars is a deadline label's budget: hseapp.LabelChars is the one copy (its label cache keys hash it).
	LabelChars      = hseapp.LabelChars
	EventTitleChars = subjects.TitleChars // an event stands in Tablo's Classes column like a subject (§2.1)

	MaterialTitleChars = 120
)

// UrgentQuiet is the seconds of silence in the chat that end an urgent burst; UrgentMaxWait the seconds from the
// trigger at most, however long the chat keeps writing. Variables: tests shorten them.
var (
	UrgentQuiet   = 60.0
	UrgentMaxWait = 300.0
)

var (
	ReadableFiles  = []string{"pdf", "document"} // a file without text still tells something by its name
	ImportantFiles = []string{"pdf", "document", "photo", "video"}
	UrgentFiles    = []string{"pdf"}
)

// Kinds are the chat kinds as the prompt names them.
var Kinds = map[string]string{"user": "private chat", "bot": "bot", "group": "group", "supergroup": "group",
	"forum": "group with topics", "channel": "channel"}

// Box is a chat of the batches with the source of its messages: the mailbox is no row of `chats`.
type Box struct {
	*inbox.Chat
	Source string
}

// MailboxChat is the mailbox as a chat of the batches: every letter is study, nothing filtered, nothing urgent (§6.2).
var MailboxChat = Box{Chat: &inbox.Chat{ID: inbox.Mailbox, Title: "University mailbox", Kind: "mail"}, Source: "mail"}

// MailboxSettings are the mailbox's settings.
func MailboxSettings() settings.Chat {
	s := settings.ChatDefaults()
	s.Enabled, s.Study = true, true
	return s
}

// Changes is what one run gave.
type Changes struct {
	News        []int64    // new items
	Deadlines   []int64    // new deadlines, Midterms too
	Moved       [][2]int64 // (old, new)
	Gone        []int64    // deadlines withdrawn with their messages or classes
	Events      []int64    // new events and corrections of classes
	EventsMoved [][2]int64 // (old, new): moved, cancelled, edited
	EventsGone  []int64    // withdrawn with their messages
	Sheets      []int64    // new sheets, waiting to be published
}

// Burst is an urgent chat waiting for its batch; monotonic seconds.
type Burst struct {
	First float64 // the trigger
	Last  float64 // the newest message since
}

// ReadyAt is when the burst is over.
func (b Burst) ReadyAt() float64 { return min(b.Last+UrgentQuiet, b.First+UrgentMaxWait) }

// Context is what the model is shown besides the messages; counted anew before every call.
type Context struct {
	News      []*news.Item
	Deadlines []*deadlines.Deadline
	Events    []*events.Event
	Classes   []schedule.UpcomingClass // a study chat only: others do not move classes
	Courses   []subjects.Named         // the dictionary
}

// message is a message of a batch with what was fetched for it.
type message struct {
	*inbox.Message
	pdf   *files.Row          // its PDF's files row, nil for none
	later bool                // the twin cannot download its file now: it waits for the next run
	image *files.ImageReading // what was read on its image
}

// Status is status().
type Status struct {
	OKAt  *string `json:"ok_at"`
	Error *string `json:"error"`
	Queue int64   `json:"queue"`
}

// Batches parses the chats.
type Batches struct {
	store        *store.Store
	db           *sql.DB
	Settings     *settings.Settings
	ChatSettings *settings.ChatSettings
	Inbox        *inbox.Inbox
	News         *news.News
	Deadlines    *deadlines.Deadlines
	Schedule     *schedule.Schedule
	Events       *events.Events
	Subjects     *subjects.Subjects
	LLM          llm.Model
	Now          func() time.Time
	Monotonic    func() float64

	Publish   func(ctx context.Context, changes Changes) error // after every run, also an empty one
	Notify    func(line i18n.Phrase)                           // a line for the owner (Web Push)
	Mail      bool                                             // mail is configured: the mailbox takes its slots
	Files     *files.Files                                     // PDFs of the messages; without it only their names
	Sheets    *sheets.Sheets                                   // where the sheets the model finds go
	Images    *images.Images                                   // reads the images of chats with «Картинки»
	Materials *feed.ChatMaterials                              // the library's queue (D58); without it none asked

	parse func(ctx context.Context, chatID int64, changes *Changes) error // ParseChat; tests replace it

	mu     sync.Mutex
	okAt   string
	err    string
	urgent map[int64]*Burst
	woken  chan struct{}
}

// New makes the batches; now nil is clock.NowUTC.
func New(s *store.Store, st *settings.Settings, chats *settings.ChatSettings, in *inbox.Inbox, nw *news.News,
	dl *deadlines.Deadlines, sch *schedule.Schedule, model llm.Model, now func() time.Time) *Batches {
	if now == nil {
		now = clock.NowUTC
	}
	started := time.Now()
	b := &Batches{store: s, db: s.DB, Settings: st, ChatSettings: chats, Inbox: in, News: nw, Deadlines: dl,
		Schedule: sch, Events: sch.Events, Subjects: sch.Subjects, LLM: model, Now: now,
		Monotonic: func() float64 { return time.Since(started).Seconds() },
		urgent:    map[int64]*Burst{}, woken: make(chan struct{}, 1)}
	b.parse = b.ParseChat
	return b
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Status is the last run, its error and the messages waiting.
func (b *Batches) Status() (Status, error) {
	counts, err := b.Inbox.PendingCounts()
	if err != nil {
		return Status{}, err
	}
	enabled, err := b.ChatSettings.EnabledIDs()
	if err != nil {
		return Status{}, err
	}
	enabled[inbox.Mailbox] = true
	var queue int64
	for chatID, n := range counts {
		if enabled[chatID] {
			queue += n
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{OKAt: nullable(b.okAt), Error: nullable(b.err), Queue: queue}, nil
}

// --- when -----------------------------------------------------------------------

// Wake is Inbox.Wake: new or pinned messages. An urgent one in an urgent chat starts a burst; any message of a chat
// in a burst stretches it.
func (b *Batches) Wake(chatID int64, msgIDs []int64) {
	now := b.Monotonic()
	b.mu.Lock()
	if burst := b.urgent[chatID]; burst != nil {
		burst.Last = now
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	chat, err := b.ChatSettings.All(chatID)
	if err != nil {
		slog.Error("waking the batches failed", "chat", chatID, "err", err)
		return
	}
	if !(chat.Enabled && chat.Urgent) {
		return
	}
	stored, err := b.Inbox.Messages(chatID, msgIDs, "tg")
	if err != nil {
		slog.Error("waking the batches failed", "chat", chatID, "err", err)
		return
	}
	for _, m := range stored {
		if m.ParsedAt == nil && m.DeletedAt == nil && Urgent(m, chat.People) {
			b.mu.Lock()
			if b.urgent[chatID] == nil {
				b.urgent[chatID] = &Burst{First: now, Last: now}
			}
			b.mu.Unlock()
			select { // the loop may be asleep until the next tick
			case b.woken <- struct{}{}:
			default:
			}
			return
		}
	}
}

// Run is the loop: the hourly ticks and the urgent chats in between, until ctx is done. Slots missed while down are
// made up at once.
func (b *Batches) Run(ctx context.Context) {
	b.guarded(func() error { _, err := b.Tick(ctx); return err })
	tickAt := b.Monotonic() + b.secondsToTick()
	for {
		select { // cleared: a wake before this point is in the bursts already
		case <-b.woken:
		default:
		}
		readyAt := tickAt
		b.mu.Lock()
		for _, burst := range b.urgent {
			readyAt = min(readyAt, burst.ReadyAt())
		}
		b.mu.Unlock()
		timer := time.NewTimer(time.Duration(max(0, readyAt-b.Monotonic()) * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-b.woken:
		case <-timer.C:
		}
		timer.Stop()
		if chats := b.readyBursts(); len(chats) > 0 {
			b.guarded(func() error {
				ordered, err := b.ordered(chats)
				if err == nil {
					_, err = b.RunChats(ctx, ordered, nil)
				}
				return err
			})
		}
		if b.Monotonic() >= tickAt {
			b.guarded(func() error { _, err := b.Tick(ctx); return err })
			tickAt = b.Monotonic() + b.secondsToTick()
		}
	}
}

// readyBursts are the urgent chats whose burst is over; they leave the waiting list.
func (b *Batches) readyBursts() []int64 {
	now := b.Monotonic()
	b.mu.Lock()
	defer b.mu.Unlock()
	var ready []int64
	for chatID, burst := range b.urgent {
		if burst.ReadyAt() <= now {
			ready = append(ready, chatID)
		}
	}
	slices.Sort(ready)
	for _, chatID := range ready {
		delete(b.urgent, chatID)
	}
	return ready
}

// Tick parses the chats whose slot has come and publishes.
func (b *Batches) Tick(ctx context.Context) (Changes, error) {
	boxes, err := b.boxes()
	if err != nil {
		return Changes{}, err
	}
	done, err := b.done()
	if err != nil {
		return Changes{}, err
	}
	due := map[int64]string{}
	for chatID := range boxes {
		slot, err := b.slot(chatID)
		if err != nil {
			return Changes{}, err
		}
		if slot > done[strconv.FormatInt(chatID, 10)] {
			due[chatID] = slot
		}
	}
	ordered, err := b.ordered(slices.Collect(maps.Keys(due)))
	if err != nil {
		return Changes{}, err
	}
	return b.RunChats(ctx, ordered, due)
}

func isLLM(err error) bool {
	var e *llm.Error
	return llm.IsBudget(err) || errors.As(err, &e)
}

// RunChats parses the chats in order, then names the courses, syncs the deadlines that follow the timetable and
// publishes. A chat's own trouble leaves its slot due and the next chats go on; the LLM's stops the run.
func (b *Batches) RunChats(ctx context.Context, chatIDs []int64, slots map[int64]string) (Changes, error) {
	changes := Changes{}
	broken := ""
	var stop error
	for _, chatID := range chatIDs {
		if err := b.parse(ctx, chatID, &changes); err != nil {
			if isLLM(err) {
				stop = err // the same for every chat: the run stops
				break
			}
			slog.Error("parsing the chat failed", "chat", chatID, "err", err)
			broken = fmt.Sprintf("chat %d: %v", chatID, err) // this chat's own trouble: its slot stays due
			continue
		}
		if slots != nil {
			if err := b.markDone(chatID, slots[chatID]); err != nil {
				return changes, err
			}
		}
	}
	switch {
	case stop == nil:
		b.mu.Lock()
		b.okAt, b.err = clock.LocalISO(b.Now()), broken
		b.mu.Unlock()
	case llm.IsBudget(stop): // the budget tells the owner itself; the chats stay due
		slog.Info("batches wait for the LLM limit", "err", stop)
	default:
		slog.Warn("batch failed, the messages stay in the queue", "err", stop)
		b.mu.Lock()
		b.err = stop.Error()
		b.mu.Unlock()
	}
	// Every run, so names left unnamed while Haiku was down get another try (§6.2); known ones cost nothing.
	today := clock.LocalDay(b.Now())
	names, err := b.Deadlines.CourseNames(today)
	if err != nil {
		return changes, err
	}
	more, err := b.Events.CourseNames(today)
	if err != nil {
		return changes, err
	}
	maps.Copy(names, more)
	if b.Sheets != nil {
		if more, err = b.Sheets.CourseNames(today); err != nil {
			return changes, err
		}
		maps.Copy(names, more)
	}
	sorted := slices.Sorted(maps.Keys(names))
	if err := b.Subjects.TryResolve(ctx, sorted, "batches"); err != nil {
		return changes, err
	}
	// A correction given, edited or withdrawn in this run moves or withdraws its interim assessment's Midterm, and
	// the assumed due dates of the sheets (§6.3) follow their seminars.
	midterms, err := b.Schedule.SyncDeadlines()
	if err != nil {
		return changes, err
	}
	changes.Deadlines = append(changes.Deadlines, midterms.Deadlines...)
	changes.Moved = append(changes.Moved, midterms.Moved...)
	changes.Gone = append(changes.Gone, midterms.Gone...)
	if b.Publish != nil {
		if err := b.Publish(ctx, changes); err != nil {
			slog.Error("publishing the batch results failed", "err", err)
		}
	}
	if b.Materials != nil { // this run's materials, and any the twin could not download before
		b.Materials.Kick(ctx)
	}
	return changes, nil
}

// boxes are what the schedule parses: the enabled chats, and the mailbox while mail is on or letters wait.
func (b *Batches) boxes() (map[int64]bool, error) {
	boxes, err := b.ChatSettings.EnabledIDs()
	if err != nil {
		return nil, err
	}
	counts, err := b.Inbox.PendingCounts()
	if err != nil {
		return nil, err
	}
	if b.Mail || counts[inbox.Mailbox] > 0 {
		boxes[inbox.Mailbox] = true
	}
	return boxes, nil
}

func (b *Batches) secondsToTick() float64 {
	now := b.Now()
	hour := now.Truncate(time.Hour)
	return hour.Add(time.Hour + TickDelay).Sub(now).Seconds()
}

// slot is the latest hour of the day this chat is parsed at, local ISO.
func (b *Batches) slot(chatID int64) (string, error) {
	every := 0
	if chatID != inbox.Mailbox {
		chat, err := b.ChatSettings.All(chatID)
		if err != nil {
			return "", err
		}
		if chat.BatchHours != nil {
			every = *chat.BatchHours
		}
	}
	if every == 0 {
		general, err := b.Settings.Int("batch_hours")
		if err != nil {
			return "", err
		}
		every = general
	}
	local := b.Now().In(clock.TZ)
	return clock.LocalISO(time.Date(local.Year(), local.Month(), local.Day(), local.Hour()-local.Hour()%every, 0, 0,
		0, clock.TZ)), nil
}

func (b *Batches) done() (map[string]string, error) {
	done := map[string]string{}
	_, err := b.store.GetJSON(DoneKey, &done)
	return done, err
}

func (b *Batches) markDone(chatID int64, slot string) error {
	done, err := b.done()
	if err != nil {
		return err
	}
	done[strconv.FormatInt(chatID, 10)] = slot
	return b.store.PutJSON(DoneKey, done)
}

// ordered: study chats, the mailbox, the other chats (D30); chats by title.
func (b *Batches) ordered(chatIDs []int64) ([]int64, error) {
	type key struct {
		rank  int
		title string
	}
	keys := map[int64]key{}
	for _, chatID := range chatIDs {
		if chatID == inbox.Mailbox {
			keys[chatID] = key{1, ""}
			continue
		}
		chat, err := b.Inbox.Chat(chatID)
		if err != nil {
			return nil, err
		}
		s, err := b.ChatSettings.All(chatID)
		if err != nil {
			return nil, err
		}
		k := key{2, ""}
		if s.Study {
			k.rank = 0
		}
		if chat != nil {
			k.title = chat.Title
		}
		keys[chatID] = k
	}
	out := slices.Clone(chatIDs)
	slices.SortStableFunc(out, func(a, c int64) int {
		ka, kc := keys[a], keys[c]
		if ka.rank != kc.rank {
			return ka.rank - kc.rank
		}
		return strings.Compare(ka.title, kc.title)
	})
	return out, nil
}

func (b *Batches) guarded(run func() error) {
	if err := run(); err != nil {
		slog.Error("batch run failed", "err", err)
	}
}

// --- one chat -------------------------------------------------------------------

// ParseChat parses the messages of one chat waiting for a batch. An *llm.Error or *llm.BudgetExceeded stops the run;
// the messages stay in the queue.
func (b *Batches) ParseChat(ctx context.Context, chatID int64, changes *Changes) error {
	chat, s, err := b.chat(chatID)
	if err != nil || chat == nil {
		return err
	}
	if err := b.retract(chat, changes); err != nil {
		return err
	}
	pending, err := b.Inbox.Pending(chatID, chat.Source)
	if err != nil || len(pending) == 0 {
		return err
	}
	chosen, skipped, err := b.choose(chatID, s, pending)
	if err != nil {
		return err
	}
	b.fetchPDFs(ctx, chosen, s.Study)
	if err := b.readImages(ctx, chat, s, chosen); err != nil { // then every message stays in the queue
		return err
	}
	var waiting []int64
	kept := chosen[:0:0]
	for _, m := range chosen {
		if m.later {
			waiting = append(waiting, m.MsgID)
		} else {
			kept = append(kept, m)
		}
	}
	if len(waiting) > 0 {
		slog.Info("messages wait for their files: the twin cannot download now", "chat", chatID, "msgs", waiting)
	}
	chosen = kept
	materials := b.materialsOn(chat, s)
	worth := func(m *message) bool { // a photo alone that was not read is not, unless it may be material
		return Readable(m.Message) || m.image != nil || (materials && feed.MaterialFile(m.Files) != nil)
	}
	kept = chosen[:0:0]
	for _, m := range chosen {
		if worth(m) {
			kept = append(kept, m)
		} else {
			skipped = append(skipped, m)
		}
	}
	chosen = kept
	if len(skipped) > 0 {
		ids := make([]int64, len(skipped))
		for i, m := range skipped {
			ids[i] = m.MsgID
		}
		if err := b.store.InTx(func(tx *sql.Tx) error {
			return b.Inbox.With(tx).MarkParsed(chatID, ids, b.Now(), chat.Source)
		}); err != nil {
			return err
		}
	}
	for _, chunk := range chunks(chosen) {
		if err := b.parseChunk(ctx, chat, s, chunk, changes); err != nil {
			return err
		}
	}
	return nil
}

// chat is the chat with its source and its settings; the mailbox is no row of `chats`.
func (b *Batches) chat(chatID int64) (*Box, settings.Chat, error) {
	if chatID == inbox.Mailbox {
		box := MailboxChat
		return &box, MailboxSettings(), nil
	}
	chat, err := b.Inbox.Chat(chatID)
	if err != nil || chat == nil {
		return nil, settings.Chat{}, err
	}
	s, err := b.ChatSettings.All(chatID)
	return &Box{Chat: chat, Source: "tg"}, s, err
}

// materialsOn: the model is asked for materials (D58): a study chat of the twin, with the library on.
func (b *Batches) materialsOn(chat *Box, s settings.Chat) bool {
	return b.Materials != nil && s.Study && chat.Source == "tg"
}

func (b *Batches) schema(chat *Box, s settings.Chat) any {
	if b.materialsOn(chat, s) {
		return StudySchema
	}
	return Schema
}

func (b *Batches) choose(chatID int64, s settings.Chat, pending []*inbox.Message) (chosen, skipped []*message,
	err error) {
	ttl, err := b.Settings.Int("news_ttl_hours")
	if err != nil {
		return nil, nil, err
	}
	oldest := clock.LocalISO(b.Now().Add(-time.Duration(ttl) * time.Hour))
	byID := map[int64]*inbox.Message{}
	for _, m := range pending {
		byID[m.MsgID] = m
	}
	imagesOn := s.Images && b.Images != nil && b.Files != nil
	materials := b.Materials != nil && s.Study && chatID != inbox.Mailbox
	for _, m := range pending {
		// A PDF may be a sheet even after a pause of the twin: it is read whatever its age (a chat's news is not).
		keep := (Readable(m) || (imagesOn && files.ImageFile(m.Files) != nil) ||
			(materials && feed.MaterialFile(m.Files) != nil)) &&
			(s.Study || inbox.KeptFrom(m) >= oldest || files.PDFFile(m.Files) != nil)
		if keep && s.Filter == "important" {
			keep = Important(m, s.People)
			if !keep {
				if keep, err = b.repliesToImportant(chatID, m, byID, s.People); err != nil {
					return nil, nil, err
				}
			}
		}
		if keep {
			chosen = append(chosen, &message{Message: m})
		} else {
			skipped = append(skipped, &message{Message: m})
		}
	}
	return chosen, skipped, nil
}

// repliesToImportant: a reply to an important message (`byID`: the messages at hand, else the stored ones) also
// passes the `important` filter; for a replied message no longer stored, its quote is checked for dates.
func (b *Batches) repliesToImportant(chatID int64, m *inbox.Message, byID map[int64]*inbox.Message,
	people []int64) (bool, error) {
	if m.ReplyTo == nil || *m.ReplyTo == 0 {
		return false, nil
	}
	replied := byID[*m.ReplyTo]
	if replied == nil {
		var err error
		if replied, err = b.Inbox.Message(chatID, *m.ReplyTo, "tg"); err != nil {
			return false, err
		}
	}
	if replied != nil {
		return Important(replied, people), nil
	}
	return HasDate(text(m.ReplyQuote)), nil
}

func sha(row *files.Row) bool { return row != nil && row.Sha256 != nil && *row.Sha256 != "" }

// fetchPDFs gets the PDF of each message, downloaded now if need be. `later`: the twin cannot download it now, the
// message waits for the next run. In a study chat and in the mailbox, a message without a PDF of its own gets the one
// its link leads to (D59).
func (b *Batches) fetchPDFs(ctx context.Context, messages []*message, study bool) {
	for _, m := range messages {
		m.pdf, m.later = nil, false
		if b.Files == nil {
			continue
		}
		row, err := b.Files.Fetch(ctx, m.Message)
		switch {
		case files.IsNotNow(err):
			m.later = true
		case err != nil: // the batch goes on with the file's name
			slog.Error("fetching the PDF of a message failed", "msg", m.MsgID, "err", err)
		default:
			m.pdf = row
		}
		if study && !m.later && !sha(m.pdf) && files.PDFFile(m.Files) == nil {
			row, err := b.Files.FetchLink(ctx, m.Message)
			if err != nil {
				slog.Error("fetching the linked PDF of a message failed", "msg", m.MsgID, "err", err)
			} else if row != nil {
				m.pdf = row
			}
		}
	}
}

// readImages reads what the image of each message shows, in a chat with «Картинки». It returns what Images.Read
// returns when the limit is spent or the model cannot be reached.
func (b *Batches) readImages(ctx context.Context, chat *Box, s settings.Chat, messages []*message) error {
	for _, m := range messages {
		m.image = nil
		if m.later || !(s.Images && b.Images != nil && b.Files != nil && files.ImageFile(m.Files) != nil) {
			continue
		}
		row, err := b.Files.FetchImage(ctx, m.Message)
		if files.IsNotNow(err) {
			m.later = true
			continue
		}
		if err != nil { // the batch goes on with the message's text
			slog.Error("fetching the image of a message failed", "msg", m.MsgID, "err", err)
			continue
		}
		if sha(row) {
			if m.image, err = b.Images.Read(ctx, chat.Chat, s.Study, m.Message, row); err != nil {
				return err
			}
		}
	}
	return nil
}

func textLimit(m *message) int {
	if m.Source == "mail" {
		return MailTextLimit
	}
	return TextLimit
}

func imageSize(m *message) int {
	if m.image == nil {
		return 0
	}
	return min(pytext.Len(m.image.Shows)+pytext.Len(m.image.Text), ImageLimit)
}

func firstPage(m *message) string {
	if m.pdf == nil {
		return ""
	}
	return text(m.pdf.FirstPage)
}

func chunks(messages []*message) [][]*message {
	var out [][]*message
	var chunk []*message
	chars := 0
	for _, m := range messages {
		size := min(pytext.Len(m.Text), textLimit(m)) + imageSize(m) + min(pytext.Len(firstPage(m)), FirstPageLimit)
		if len(chunk) > 0 && (len(chunk) >= ChunkMsgs || chars+size > ChunkChars) {
			out = append(out, chunk)
			chunk, chars = nil, 0
		}
		chunk = append(chunk, m)
		chars += size
	}
	if len(chunk) > 0 {
		out = append(out, chunk)
	}
	return out
}

// pdfs are the messages whose PDF was read: only they may be sheets.
func pdfs(messages []*message) map[int64]bool {
	out := map[int64]bool{}
	for _, m := range messages {
		if sha(m.pdf) {
			out[m.MsgID] = true
		}
	}
	return out
}

func scanned(messages []*message) []*message {
	var out []*message
	for _, m := range messages {
		if sha(m.pdf) && pdf.Scanned(firstPage(m)) {
			out = append(out, m)
		}
	}
	return out
}

func ids(messages []*message) map[int64]bool {
	out := map[int64]bool{}
	for _, m := range messages {
		out[m.MsgID] = true
	}
	return out
}

func (b *Batches) context(chat *Box, s settings.Chat, started time.Time) (*Context, error) {
	today := clock.LocalDay(started)
	ttl, err := b.Settings.Int("news_ttl_hours")
	if err != nil {
		return nil, err
	}
	c := &Context{}
	if c.News, err = b.News.Live(started, ttl); err != nil {
		return nil, err
	}
	if c.Deadlines, err = b.Deadlines.Upcoming(today, PassedDeadlines); err != nil {
		return nil, err
	}
	if c.Events, err = b.Events.Upcoming(today); err != nil {
		return nil, err
	}
	if s.Study {
		if c.Classes, err = b.Schedule.UpcomingClasses(today); err != nil {
			return nil, err
		}
	}
	if c.Courses, err = b.Subjects.All(); err != nil {
		return nil, err
	}
	if !s.Study { // D79: a chat that is no study one moves, cancels and replaces only its own items
		own := func(source string, chatID *int64) bool {
			return source == chat.Source && chatID != nil && *chatID == chat.ID
		}
		c.News = slices.DeleteFunc(c.News, func(n *news.Item) bool { return !own(n.Source, &n.ChatID) })
		c.Deadlines = slices.DeleteFunc(c.Deadlines, func(d *deadlines.Deadline) bool { return !own(d.Source, d.ChatID) })
		c.Events = slices.DeleteFunc(c.Events, func(e *events.Event) bool { return !own(e.Source, e.ChatID) })
	}
	return c, nil
}

func (b *Batches) ask(ctx context.Context, chat *Box, s settings.Chat, messages []*message, c *Context,
	notes map[int64]string) ([]Item, error) {
	documents, err := b.scans(messages) // before the prompt: a scan that cannot be attached is not referred to
	if err != nil {
		return nil, err
	}
	prompt, err := b.prompt(chat, s, messages, c, notes)
	if err != nil {
		return nil, err
	}
	chatID := chat.ID
	raw, err := b.LLM.JSON(ctx, llm.Request{Task: "batch", System: System, Prompt: prompt, Schema: b.schema(chat, s),
		MaxTokens: MaxTokens, ChatID: &chatID, Documents: documents})
	if err != nil {
		return nil, err
	}
	answer, err := decode(raw)
	if err != nil {
		return nil, llm.NewError(llm.BadAnswer, "the answer is no JSON object: "+err.Error())
	}
	return Check(answer, ids(messages), pdfs(messages), b.materialsOn(chat, s)), nil
}

func (b *Batches) parseChunk(ctx context.Context, chat *Box, s settings.Chat, chunk []*message,
	changes *Changes) error {
	started := b.Now() // an edit during the call is newer than parsed_at and comes back
	c, err := b.context(chat, s, started)
	if err != nil {
		return err
	}
	items, err := b.ask(ctx, chat, s, chunk, c, nil)
	if llm.IsBadAnswer(err) {
		if len(chunk) > 1 { // find the message that breaks it; the others go through
			half := len(chunk) / 2
			if err := b.parseChunk(ctx, chat, s, chunk[:half], changes); err != nil {
				return err
			}
			return b.parseChunk(ctx, chat, s, chunk[half:], changes)
		}
		return b.skip(chat, chunk[0], err.Error())
	}
	if err != nil {
		return err
	}
	if slices.ContainsFunc(items, func(i Item) bool { return len(TooLong(i)) > 0 }) {
		if items, err = b.retry(ctx, chat, s, chunk, c, items); err != nil {
			return err
		}
	}
	for i := range items {
		Fit(&items[i])
	}
	return b.save(chat, s, chunk, items, c, started, changes)
}

func overlaps(a []int64, set map[int64]bool) bool {
	return slices.ContainsFunc(a, func(id int64) bool { return set[id] })
}

// retry is one more call for the messages of the items that do not fit, with the reasons (§6.4); on failure the
// first answer stays. The new answer stands for every item of those messages: a message may give several items, and
// the new ones need not come in the old order or number, so an old item kept beside them would be told twice.
func (b *Batches) retry(ctx context.Context, chat *Box, s settings.Chat, chunk []*message, c *Context,
	items []Item) ([]Item, error) {
	notes := map[int64]string{}
	again := map[int64]bool{}
	for _, item := range items {
		if reasons := TooLong(item); len(reasons) > 0 {
			notes[item.MsgIDs[0]] = strings.Join(reasons, "; ")
			for _, id := range item.MsgIDs {
				again[id] = true
			}
		}
	}
	for grown := true; grown; { // an item that shares a message with them is answered anew too
		grown = false
		for _, item := range items {
			if overlaps(item.MsgIDs, again) {
				for _, id := range item.MsgIDs {
					if !again[id] {
						again[id], grown = true, true
					}
				}
			}
		}
	}
	var messages []*message
	for _, m := range chunk {
		if again[m.MsgID] {
			messages = append(messages, m)
		}
	}
	retried, err := b.ask(ctx, chat, s, messages, c, notes)
	if err != nil {
		if isLLM(err) {
			slog.Info("the retry for length failed, the first answer stays", "err", err)
			return items, nil
		}
		return nil, err
	}
	if len(retried) == 0 {
		return items, nil
	}
	var result []Item
	for _, item := range items {
		if overlaps(item.MsgIDs, again) {
			result = append(result, retried...) // in place of the first of them
			retried = nil
		} else {
			result = append(result, item)
		}
	}
	return result, nil
}

// skip is a message the model cannot answer for. It stays in the queue for the next runs of its chat (an answer cut
// short or malformed may come right next time, and a PDF in it may be a sheet), and after SkipAfter failed runs it
// leaves the queue and the owner hears about it.
func (b *Batches) skip(chat *Box, m *message, reason string) error {
	key := fmt.Sprintf("%s:%d:%d", chat.Source, chat.ID, m.MsgID)
	failed := map[string]int{}
	if _, err := b.store.GetJSON(FailedKey, &failed); err != nil {
		return err
	}
	failed[key]++
	if failed[key] < SkipAfter {
		slog.Warn("a message failed, it is tried again at the next run", "msg", m.MsgID, "chat", chat.ID,
			"failed", failed[key], "of", SkipAfter, "err", reason)
		return b.store.PutJSON(FailedKey, failed)
	}
	delete(failed, key)
	slog.Warn("a message is skipped", "msg", m.MsgID, "chat", chat.ID, "err", reason)
	value, err := pyjson.DumpsASCII(failed)
	if err != nil {
		return err
	}
	if err := b.store.InTx(func(tx *sql.Tx) error {
		if err := b.Inbox.With(tx).MarkParsed(chat.ID, []int64{m.MsgID}, b.Now(), chat.Source); err != nil {
			return err
		}
		_, err := tx.Exec("insert into kv(key, value) values(?, ?) on conflict(key) do update set "+
			"value = excluded.value", FailedKey, value)
		return err
	}); err != nil {
		return err
	}
	if b.Notify == nil {
		return nil
	}
	pdfEn, pdfRu := "", ""
	if sha(m.pdf) {
		name := pyOptional(m.pdf.Name)
		pdfEn = fmt.Sprintf(" Its PDF «%s» may be a sheet: add it by hand in the web app, Sheets.", name)
		pdfRu = fmt.Sprintf(" Его PDF «%s» может быть листочком: добавьте его вручную в приложении, «Листочки».", name)
	}
	if chat.Source == "mail" {
		sender := pyOptional(m.SenderName)
		subjectEn, subjectRu := text(m.Subject), text(m.Subject)
		if subjectEn == "" {
			subjectEn, subjectRu = "no subject", "без темы"
		}
		b.Notify(i18n.P(fmt.Sprintf("The letter «%s» (%s) is skipped by the parsing: %s.%s", subjectEn, sender, reason,
			pdfEn), fmt.Sprintf("Письмо «%s» (%s) пропущено разбором: %s.%s", subjectRu, sender, reason, pdfRu)))
		return nil
	}
	tail := ""
	if link := inbox.MessageLink(chat.Chat, m.MsgID); link != "" {
		tail = "\n" + link
	}
	b.Notify(i18n.P(fmt.Sprintf("A message of «%s» is skipped by the parsing: %s.%s%s", chat.Title, reason, pdfEn, tail),
		fmt.Sprintf("Сообщение из «%s» пропущено разбором: %s.%s%s", chat.Title, reason, pdfRu, tail)))
	return nil
}

// pyOptional is a value as an f-string prints it: None for none.
func pyOptional[T any](p *T) string {
	if p == nil {
		return "None"
	}
	return fmt.Sprint(*p)
}

// --- the prompt -------------------------------------------------------------------

func (b *Batches) prompt(chat *Box, s settings.Chat, messages []*message, c *Context,
	notes map[int64]string) (string, error) {
	now := b.Now().In(clock.TZ)
	header := MailboxHeader
	if chat.Source != "mail" {
		kind, ok := Kinds[chat.Kind]
		if !ok {
			kind = chat.Kind
		}
		study := "not a study chat"
		if s.Study {
			study = "study"
		}
		header = fmt.Sprintf("Chat: \"%s\" (%s, %s)", chat.Title, kind, study)
	}
	today := now.Format(clock.DateLayout)
	lines := []string{"Today: " + now.Format("Mon 2006-01-02 15:04"),
		fmt.Sprintf("Budgets: full at most 2 lines of %d characters; short at most %d characters; deadline label at "+
			"most %d characters; event title at most %d characters.", Line, Line, LabelChars-1, EventTitleChars),
		""}
	about, err := b.Settings.String("about_me")
	if err != nil {
		return "", err
	}
	if about != "" {
		lines = append(lines, "About the student:")
		lines = append(lines, SplitLines(about)...)
		lines = append(lines, "")
	}
	lines = append(lines, header)
	if s.Note != "" {
		lines = append(lines, "Note on the chat:")
		lines = append(lines, SplitLines(s.Note)...)
	}
	lines = append(lines, "", "Course dictionary (abbreviation \"title\": the names it goes by):")
	lines = append(lines, orNone(subjects.DictionaryLines(c.Courses), "(empty)")...)
	lines = append(lines, "", "Current news:")
	var part []string
	for _, n := range c.News {
		part = append(part, fmt.Sprintf("- n%d: \"%s\"", n.ID, n.Full))
	}
	lines = append(lines, orNone(part, "(none)")...)
	lines = append(lines, "", "Deadlines (upcoming and recently passed):")
	part = nil
	for _, d := range c.Deadlines {
		line := fmt.Sprintf("- d%d: \"%s\" %s", d.ID, d.Label, d.Due)
		if d.Due < today {
			line += " passed"
		}
		if d.Assumed {
			line += " assumed"
		}
		part = append(part, line)
	}
	lines = append(lines, orNone(part, "(none)")...)
	lines = append(lines, "", "Upcoming events:")
	part = nil
	for _, e := range c.Events {
		part = append(part, EventLine(e))
	}
	lines = append(lines, orNone(part, "(none)")...)
	if s.Study {
		lines = append(lines, "", "Timetable classes:")
		part = nil
		for _, item := range c.Classes {
			part = append(part, ClassLine(item))
		}
		lines = append(lines, orNone(part, "(none)")...)
	}
	byID := map[int64]bool{}
	for _, m := range messages {
		byID[m.MsgID] = true
	}
	earlier, err := b.earlier(chat, messages)
	if err != nil {
		return "", err
	}
	if len(earlier) > 0 {
		lines = append(lines, "", "Earlier messages, for context only (no items, their ids not in msg_ids):")
		for _, m := range earlier {
			lines = append(lines, earlierLines(m, s.People)...)
			byID[m.MsgID] = true // a reply to one of them is shown as such, without a quote
		}
	}
	lines = append(lines, "", "New messages:")
	albumFirst := map[int64]int64{}
	for _, m := range messages {
		if m.GroupedID != nil && *m.GroupedID != 0 {
			if _, ok := albumFirst[*m.GroupedID]; !ok {
				albumFirst[*m.GroupedID] = m.MsgID
			}
		}
	}
	scans := map[int64]int{}
	for n, m := range scanned(messages) {
		scans[m.MsgID] = n + 1
	}
	for _, m := range messages {
		rendered, err := b.render(chat, m, byID, albumFirst, s.People, notes[m.MsgID], scans)
		if err != nil {
			return "", err
		}
		lines = append(lines, rendered...)
	}
	return strings.Join(lines, "\n"), nil
}

func orNone(lines []string, none string) []string {
	if len(lines) == 0 {
		return []string{none}
	}
	return lines
}

// earlier are the messages of the chat just before the first new one (ContextMessages), none for the mailbox.
func (b *Batches) earlier(chat *Box, messages []*message) ([]*inbox.Message, error) {
	if chat.Source == "mail" || len(messages) == 0 {
		return nil, nil
	}
	first := messages[0]
	at, err := clock.ParseLocal(first.At)
	if err != nil {
		return nil, err
	}
	before, err := b.Inbox.Before(chat.ID, first.Message, at.Add(-ContextAge), ContextMessages)
	if err != nil {
		return nil, err
	}
	isNew := ids(messages)
	return slices.DeleteFunc(before, func(m *inbox.Message) bool { return isNew[m.MsgID] }), nil
}

func at(text string) string {
	t, _ := clock.ParseLocal(text)
	return t.Format("Mon 2006-01-02 15:04")
}

func fileNames(fs []inbox.File) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		name, _ := f["name"].(string)
		names[i] = Quoted(&name)
		if names[i] == "" {
			names[i] = kind(f)
		}
	}
	return "files: " + strings.Join(names, ", ")
}

func sender(m *inbox.Message) string {
	if q := Quoted(m.SenderName); q != "" {
		return q
	}
	return "unknown sender"
}

func earlierLines(m *inbox.Message, people []int64) []string {
	head := []string{strconv.FormatInt(m.MsgID, 10), sender(m)}
	if listed(m, people) {
		head = append(head, "listed person")
	}
	head = append(head, at(m.At))
	if m.ReplyTo != nil && *m.ReplyTo != 0 {
		head = append(head, fmt.Sprintf("reply to %d", *m.ReplyTo))
	}
	if len(m.Files) > 0 {
		head = append(head, fileNames(m.Files))
	}
	t := squeeze(m.Text)
	if pytext.Len(t) > ContextChars {
		t = strings.TrimRightFunc(pytext.Cut(t, ContextChars-1), pytext.IsSpace) + "…"
	}
	lines := []string{"- " + strings.Join(head, " · ")}
	if t != "" {
		lines = append(lines, "  "+t)
	}
	return lines
}

// scans are the first pages of the scanned PDFs, in the order of the prompt. A file that cannot be attached (gone
// meanwhile) leaves the message with its file's name only.
func (b *Batches) scans(messages []*message) ([]llm.Document, error) {
	var documents []llm.Document
	for _, m := range scanned(messages) {
		path := ""
		if b.Files != nil {
			path = b.Files.Path(m.pdf)
		}
		var head []byte
		var err error
		if path == "" {
			err = &pdf.PdfError{Msg: "the file is gone"}
		} else {
			head, err = pdf.Head(path, 1)
		}
		var p *pdf.PdfError
		switch {
		case errors.As(err, &p):
			slog.Warn("the first page is not attached", "name", text(m.pdf.Name), "err", err)
			m.pdf = nil
		case err != nil:
			return nil, err
		default:
			documents = append(documents, llm.Document{Data: head, Pages: 1})
		}
	}
	return documents, nil
}

func (b *Batches) render(chat *Box, m *message, byID map[int64]bool, albumFirst map[int64]int64, people []int64,
	note string, scans map[int64]int) ([]string, error) {
	head := []string{strconv.FormatInt(m.MsgID, 10), sender(m.Message)}
	if listed(m.Message, people) {
		head = append(head, "listed person")
	}
	head = append(head, at(m.At))
	if text(m.EditedAt) != "" { // relative dates of the text count from the edit
		head = append(head, "edited "+at(*m.EditedAt))
	}
	if m.Pinned {
		head = append(head, "pinned")
	}
	if m.GroupedID != nil && *m.GroupedID != 0 {
		if first, ok := albumFirst[*m.GroupedID]; ok && first != m.MsgID {
			head = append(head, fmt.Sprintf("album with %d", first))
		}
	}
	if len(m.Forward) > 0 {
		from, _ := m.Forward["from"].(string)
		q := Quoted(&from)
		if q == "" {
			q = "unknown"
		}
		head = append(head, "forwarded from "+q)
	}
	replyTo := int64(0)
	if m.ReplyTo != nil {
		replyTo = *m.ReplyTo
	}
	if replyTo != 0 {
		if byID[replyTo] {
			head = append(head, fmt.Sprintf("reply to %d", replyTo))
		} else {
			head = append(head, "reply")
		}
	}
	if len(m.Files) > 0 {
		head = append(head, fileNames(m.Files))
	}
	lines := []string{"- " + strings.Join(head, " · ")}
	if text(m.Subject) != "" {
		lines = append(lines, "  Subject: "+squeeze(*m.Subject))
	}
	var body []string
	for _, line := range SplitLines(pytext.Cut(m.Text, textLimit(m))) {
		if pytext.Strip(line) != "" {
			body = append(body, "  "+line)
		}
	}
	lines = append(lines, orNone(body, "  (no text)")...)
	if sha(m.pdf) {
		about := fmt.Sprintf("  PDF \"%s\", %s pages", pyOptional(m.pdf.Name), pyOptional(m.pdf.Pages))
		if text(m.pdf.URL) != "" {
			about += ", downloaded from the link " + *m.pdf.URL
		}
		if pdf.Scanned(firstPage(m)) {
			lines = append(lines, fmt.Sprintf("%s: a scan, its first page is attached as document %d", about,
				scans[m.MsgID]))
		} else {
			lines = append(lines, about+", first page:")
			for _, line := range SplitLines(pytext.Cut(firstPage(m), FirstPageLimit)) {
				lines = append(lines, "  | "+line)
			}
		}
	}
	if m.image != nil {
		shows := m.image.Shows
		if shows == "" {
			shows = "no description"
		}
		lines = append(lines, "  Image, as another model read it: "+shows)
		for _, line := range SplitLines(pytext.Cut(m.image.Text, ImageLimit)) {
			lines = append(lines, "  | "+line)
		}
	}
	if replyTo != 0 && !byID[replyTo] {
		replied, err := b.Inbox.Message(chat.ID, replyTo, "tg")
		if err != nil {
			return nil, err
		}
		quote := ""
		if replied != nil {
			quote = replied.Text
		}
		if quote == "" {
			quote = text(m.ReplyQuote)
		}
		if quote = pytext.Cut(squeeze(quote), QuoteLimit); quote != "" {
			lines = append(lines, "  > replied message: «"+quote+"»")
		}
	}
	if Reread(m.Message) {
		made, err := b.News.MadeOf(chat.Source, chat.ID, m.MsgID)
		if err != nil {
			return nil, err
		}
		earlier := make([]string, len(made))
		for i, n := range made {
			earlier[i] = fmt.Sprintf("n%d", n.ID)
		}
		if len(earlier) > 0 || m.ParsedAt != nil {
			line := "  (edited after it was read"
			if len(earlier) > 0 {
				line += "; its earlier news: " + strings.Join(earlier, ", ")
			}
			lines = append(lines, line+")")
		}
	}
	if note != "" {
		lines = append(lines, "  Your previous answer did not fit: "+note+". Write it shorter.")
	}
	return lines, nil
}

// --- saving -----------------------------------------------------------------------

// tx is the repositories bound to one transaction of a save.
type tx struct {
	q     *sql.Tx
	news  *news.News
	dl    *deadlines.Deadlines
	ev    *events.Events
	inbox *inbox.Inbox
	view  *schedule.Schedule // the schedule's lookups through the transaction
}

func (b *Batches) bind(q *sql.Tx) *tx {
	view := *b.Schedule
	view.Events, view.Deadlines = b.Events.With(q), b.Deadlines.With(q)
	view.Subjects, view.Inbox = b.Subjects.With(q), b.Inbox.With(q)
	return &tx{q: q, news: b.News.With(q), dl: view.Deadlines, ev: view.Events, inbox: view.Inbox, view: &view}
}

// classEntries is Schedule.ClassEntries read through the transaction: the corrections saved so far in it count.
func (t *tx) classEntries() ([]schedule.Entry, error) {
	corrections, err := t.ev.Corrections()
	if err != nil {
		return nil, err
	}
	rows, err := ruz.Rows(t.q, "where gone_at is null and start >= ? and start < ? order by start, subject", "",
		"9999")
	if err != nil {
		return nil, err
	}
	out := make([]schedule.Entry, 0, len(rows))
	for _, item := range rows {
		entry, err := t.view.ClassEntry(item, corrections[item.UID])
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

type saving struct {
	chat          *Box
	ids           []int64
	c             *Context
	oldEvents     []*events.Event // what edited messages gave before, in order
	settledEvents map[int64]bool
	now           time.Time
	changes       *Changes
}

func (b *Batches) save(chat *Box, s settings.Chat, chunk []*message, items []Item, c *Context, started time.Time,
	changes *Changes) error {
	chatID, source, now := chat.ID, chat.Source, b.Now()
	byID := map[int64]*message{}
	for _, m := range chunk {
		byID[m.MsgID] = m
	}
	knownNews, knownDeadlines := map[string]int64{}, map[string]int64{}
	for _, n := range c.News {
		knownNews[fmt.Sprintf("n%d", n.ID)] = n.ID
	}
	for _, d := range c.Deadlines {
		knownDeadlines[fmt.Sprintf("d%d", d.ID)] = d.ID
	}
	// What edited messages gave before: replaced by their new items, the rest withdrawn.
	type oldNews struct {
		msg int64
		ids []int64
	}
	var olds []oldNews
	var oldDeadlines []*deadlines.Deadline
	var oldEvents []*events.Event
	seenD, seenE := map[int64]bool{}, map[int64]bool{}
	for _, m := range chunk {
		if !Reread(m.Message) {
			continue
		}
		made, err := b.News.MadeOf(source, chatID, m.MsgID)
		if err != nil {
			return err
		}
		entry := oldNews{msg: m.MsgID}
		for _, n := range made {
			entry.ids = append(entry.ids, n.ID)
		}
		olds = append(olds, entry)
		ds, err := b.Deadlines.MadeOf(source, chatID, m.MsgID)
		if err != nil {
			return err
		}
		for _, d := range ds {
			if !seenD[d.ID] {
				seenD[d.ID] = true
				oldDeadlines = append(oldDeadlines, d)
			}
		}
		es, err := b.Events.MadeOf(source, chatID, m.MsgID)
		if err != nil {
			return err
		}
		for _, e := range es {
			if !seenE[e.ID] {
				seenE[e.ID] = true
				oldEvents = append(oldEvents, e)
			}
		}
	}
	settledNews, settledDeadlines := map[int64]bool{}, map[int64]bool{}
	sv := &saving{chat: chat, c: c, oldEvents: oldEvents, settledEvents: map[int64]bool{}, now: now, changes: changes}
	batchNews := map[string]int64{} // "m<msg_id>" of this batch -> its news item
	ordinals := map[int64]int{}     // first message of an item -> its events so far: their UIDs differ
	var materials []feed.Material
	err := b.store.InTx(func(q *sql.Tx) error {
		t := b.bind(q)
		for _, item := range items {
			ids := item.MsgIDs
			// D79: a letter from outside the university adds; it moves, cancels and replaces nothing known.
			trusted := source != "mail" || !slices.ContainsFunc(ids, func(i int64) bool {
				return !Official(byID[i].Message)
			})
			if !trusted {
				if item.News != nil {
					n := *item.News
					n.Replaces = ""
					item.News = &n
				}
				ds := make([]Deadline, len(item.Deadlines))
				for i, d := range item.Deadlines {
					d.Moves = ""
					ds[i] = d
				}
				item.Deadlines = ds
				var evs []Event
				for _, e := range item.Events {
					if e.Moves == "" && e.Cancels == "" {
						evs = append(evs, e)
					}
				}
				item.Events = evs
			}
			if n := item.News; n != nil {
				posted := ""
				for _, i := range ids {
					posted = max(posted, byID[i].At)
				}
				newsID, err := t.news.Add(news.New{Source: source, ChatID: chatID, MsgIDs: ids, Study: s.Study,
					PostedAt: posted, Full: n.Full, Short: n.Short, Importance: int64(n.Importance)}, now)
				if err != nil {
					return err
				}
				changes.News = append(changes.News, newsID)
				target, ok := knownNews[n.Replaces]
				if !ok {
					target, ok = batchNews[n.Replaces]
				}
				if ok {
					if err := t.news.Replace(target, newsID); err != nil {
						return err
					}
					settledNews[target] = true
				}
				for _, old := range olds {
					if !slices.Contains(ids, old.msg) {
						continue
					}
					for _, id := range old.ids {
						if err := t.news.Replace(id, newsID); err != nil {
							return err
						}
						settledNews[id] = true
					}
				}
				for _, i := range ids {
					batchNews[fmt.Sprintf("m%d", i)] = newsID
				}
			}
			for _, d := range item.Deadlines {
				same, err := t.dl.Find(d.Label, d.Due)
				if err != nil {
					return err
				}
				if same != nil { // known already, maybe from the earlier text of this very message
					if same.Assumed { // a sheet's assumed date, told for real now (§6.3)
						if err := t.dl.Confirm(same.ID); err != nil {
							return err
						}
					}
					settledDeadlines[same.ID] = true
					continue
				}
				newID, err := t.dl.Add(deadlines.New{Source: source, ChatID: &chatID, MsgIDs: ids, Course: d.Course,
					Label: d.Label, Due: d.Due}, now)
				if err != nil {
					return err
				}
				moved, ok := knownDeadlines[d.Moves]
				if !ok { // an edit that changed the date moves the deadline of the earlier text
					for _, old := range oldDeadlines {
						if !settledDeadlines[old.ID] && overlapsIDs(old.MsgIDs, ids) &&
							deadlines.SameLabel(old.Label, d.Label) {
							moved, ok = old.ID, true
							break
						}
					}
				}
				done := false
				if ok {
					if done, err = t.dl.Move(moved, newID); err != nil {
						return err
					}
				}
				if done {
					settledDeadlines[moved] = true
					changes.Moved = append(changes.Moved, [2]int64{moved, newID})
				} else {
					changes.Deadlines = append(changes.Deadlines, newID)
				}
			}
			sv.ids = ids
			for _, e := range item.Events {
				n := 0
				if e.Moves == "" && e.Cancels == "" {
					prev, seen := ordinals[ids[0]]
					if !seen {
						prev = -1
					}
					n = prev + 1
					ordinals[ids[0]] = n
				}
				if err := b.saveEvent(t, sv, e, n, s.Study && trusted); err != nil {
					return err
				}
			}
			for _, found := range item.Sheets {
				if b.Sheets == nil {
					continue
				}
				m := byID[found.MsgID]
				sheetID, err := b.Sheets.Add(q, chatID, m.MsgID, m.At, m.pdf, found, now)
				if err != nil {
					return err
				}
				if sheetID != nil {
					changes.Sheets = append(changes.Sheets, *sheetID)
				}
			}
		}
		for _, item := range items {
			materials = append(materials, item.Materials...)
		}
		if len(materials) > 0 && b.materialsOn(chat, s) { // taken after the run: files are downloaded then
			if err := feed.Queue(q, chatID, materials, now); err != nil {
				return err
			}
		}
		var goneNews, goneDeadlines, goneEvents []int64
		for _, old := range olds {
			for _, id := range old.ids {
				if !settledNews[id] {
					goneNews = append(goneNews, id)
				}
			}
		}
		for _, old := range oldDeadlines {
			if !settledDeadlines[old.ID] {
				goneDeadlines = append(goneDeadlines, old.ID)
			}
		}
		// A correction of a class the model was not shown (the chat is no longer a study one, the class is past)
		// could not be restated: it stays.
		shown := map[string]bool{}
		for _, item := range c.Classes {
			shown[item.UID] = true
		}
		for _, old := range oldEvents {
			if !sv.settledEvents[old.ID] && (old.ClassUID == nil || shown[*old.ClassUID]) {
				goneEvents = append(goneEvents, old.ID)
			}
		}
		if len(goneNews) > 0 {
			if err := t.news.Drop(goneNews, now); err != nil {
				return err
			}
		}
		if len(goneDeadlines) > 0 {
			if _, err := t.dl.Drop(goneDeadlines, now); err != nil {
				return err
			}
			changes.Gone = append(changes.Gone, goneDeadlines...)
		}
		if len(goneEvents) > 0 {
			if _, err := t.ev.Drop(goneEvents, now); err != nil {
				return err
			}
			changes.EventsGone = append(changes.EventsGone, goneEvents...)
		}
		all := make([]int64, len(chunk))
		for i, m := range chunk {
			all[i] = m.MsgID
		}
		return t.inbox.MarkParsed(chatID, all, started, source)
	})
	if err != nil {
		return err
	}
	newsN, deadlinesN, eventsN, sheetsN := 0, 0, 0, 0
	for _, item := range items {
		if item.News != nil {
			newsN++
		}
		deadlinesN += len(item.Deadlines)
		eventsN += len(item.Events)
		sheetsN += len(item.Sheets)
	}
	slog.Info("batch", "chat", chatID, "messages", len(chunk), "news", newsN, "deadlines", deadlinesN,
		"events", eventsN, "sheets", sheetsN, "materials", len(materials))
	return nil
}

func overlapsIDs(a, b []int64) bool {
	return slices.ContainsFunc(a, func(id int64) bool { return slices.Contains(b, id) })
}

func ptrText(p *string) string { return text(p) }

func local(s string) time.Time {
	t, _ := clock.ParseLocal(s)
	return t
}

func isoLocal(t time.Time) string { return t.In(clock.TZ).Format(clock.LocalLayout) }

// saveEvent is one event of the answer: a new version of a listed event, a correction of a listed class, or an event
// of its own. Inside the transaction of save.
func (b *Batches) saveEvent(t *tx, sv *saving, e Event, n int, joins bool) error {
	chat, now, changes, settled := sv.chat, sv.now, sv.changes, sv.settledEvents
	chatID := chat.ID
	told := func(ev *events.New) { ev.Source, ev.ChatID, ev.MsgIDs = chat.Source, &chatID, sv.ids }
	target := e.Cancels
	if target == "" {
		target = e.Moves
	}
	var listed *events.Event
	for _, ev := range sv.c.Events {
		if fmt.Sprintf("e%d", ev.ID) == target {
			listed = ev
			break
		}
	}
	if listed != nil {
		if listed.Source == events.Own {
			return nil // the owner's own: not a chat's to move
		}
		current, err := t.ev.Current(listed.ID) // another message of this batch may have moved it already
		if err != nil || current == nil || current.GoneAt != nil {
			return err
		}
		fresh := copyOf(current)
		told(&fresh)
		if e.Cancels != "" {
			fresh.Cancelled = true
		} else {
			start, end := onDay(e.Start, current.Start), e.End
			if end == "" && current.EndText() != "" && !schedule.AllDay(start) { // the same length as before
				end = isoLocal(local(start).Add(local(*current.End).Sub(local(current.Start))))
			}
			fresh.Start, fresh.End, fresh.Cancelled = start, end, false
			if e.Room != "" {
				fresh.Room = e.Room
			}
		}
		if fresh.Start == current.Start && fresh.End == current.EndText() && fresh.Room == current.Room &&
			fresh.Cancelled == current.Cancelled {
			settled[current.ID] = true // told again, nothing new
			return nil
		}
		newID, err := t.ev.Add(fresh, now)
		if err != nil {
			return err
		}
		if _, err := t.ev.Supersede(current.ID, newID); err != nil {
			return err
		}
		settled[current.ID] = true
		changes.EventsMoved = append(changes.EventsMoved, [2]int64{current.ID, newID})
		return nil
	}

	var class *schedule.UpcomingClass
	for i, item := range sv.c.Classes {
		if fmt.Sprintf("c%d", item.ID) == target {
			class = &sv.c.Classes[i]
			break
		}
	}
	if class != nil {
		if class.Cancelled {
			return nil // RUZ has cancelled it itself
		}
		start, end, room := class.Start, class.End, class.Room
		earlier, err := t.ev.Correction(class.UID)
		if err != nil {
			return err
		}
		if e.Moves != "" {
			start = onDay(e.Start, class.Start)
			if e.Room != "" {
				room = e.Room
			}
			end = e.End
			if end == "" {
				end = isoLocal(local(start).Add(local(class.End).Sub(local(class.Start))))
			}
			if start == class.Start && end == class.End && room == class.Room && earlier == nil {
				return nil // the class where it always is: no move (with a correction in force, it is a move back)
			}
		}
		cancelled := e.Cancels != ""
		if earlier != nil && earlier.Cancelled == cancelled && earlier.Start == start && earlier.EndText() == end &&
			earlier.Room == room {
			settled[earlier.ID] = true
			return nil
		}
		fresh := events.New{UID: class.UID, ClassUID: class.UID, Course: class.Subject, Title: class.Title,
			Kind: class.Kind, Start: start, End: end, Room: room, Required: true, Cancelled: cancelled}
		told(&fresh)
		newID, err := t.ev.Add(fresh, now)
		if err != nil {
			return err
		}
		if earlier != nil {
			if _, err := t.ev.Supersede(earlier.ID, newID); err != nil {
				return err
			}
			settled[earlier.ID] = true
		}
		changes.Events = append(changes.Events, newID)
		return nil
	}

	if e.Cancels != "" || strings.HasPrefix(e.Moves, "c") || e.Start == "" || e.Title == "" {
		return nil // cancels or moves something that was not listed: nothing to apply it to
	}
	uid := events.UID(chat.Source, chat.ID, sv.ids[0], n)
	fresh := events.New{UID: uid, Course: e.Course, Title: e.Title, Kind: e.Kind, Start: e.Start, End: e.End,
		Room: e.Room, Required: e.Required}
	told(&fresh)
	var earlier *events.Event
	for _, old := range sv.oldEvents {
		if !settled[old.ID] && old.UID == uid {
			earlier = old
			break
		}
	}
	if earlier != nil { // the message was edited: its event again, maybe changed
		settled[earlier.ID] = true
		if fresh.Title == earlier.Title && fresh.Start == earlier.Start && fresh.End == earlier.EndText() &&
			fresh.Room == earlier.Room && !earlier.Cancelled {
			return nil
		}
		newID, err := t.ev.Add(fresh, now)
		if err != nil {
			return err
		}
		if _, err := t.ev.Supersede(earlier.ID, newID); err != nil {
			return err
		}
		changes.EventsMoved = append(changes.EventsMoved, [2]int64{earlier.ID, newID})
		return nil
	}
	same, err := t.ev.Find(e.Title, e.Start)
	if err != nil {
		return err
	}
	if same != nil {
		settled[same.ID] = true // known already, maybe from the earlier text of this very message
		return nil
	}
	if byUID, err := t.ev.ByUID(uid); err != nil || byUID != nil {
		return err // another message has moved this event since: this text no longer tells it
	}
	entries, err := t.classEntries()
	if err != nil {
		return err
	}
	repeats, err := t.view.RepeatsClass(&events.Event{UID: uid, Source: fresh.Source, ChatID: fresh.ChatID,
		MsgIDs: fresh.MsgIDs, Course: nullable(fresh.Course), Title: fresh.Title, Kind: fresh.Kind, Start: fresh.Start,
		End: nullable(fresh.End), Room: fresh.Room, Required: fresh.Required}, entries)
	if err != nil {
		return err
	}
	if repeats {
		slog.Info("an event repeats a class: not saved", "title", fresh.Title, "start", fresh.Start, "chat", chat.ID)
		return nil
	}
	twin, err := dayTwin(t.ev, fresh)
	if err != nil {
		return err
	}
	if twin != nil && (joins || (twin.Source == chat.Source && twin.ChatID != nil && *twin.ChatID == chat.ID)) {
		return join(t.ev, twin, fresh, settled, now, changes) // another chat's event: only a trusted study one
	}
	newID, err := t.ev.Add(fresh, now)
	if err != nil {
		return err
	}
	changes.Events = append(changes.Events, newID)
	return nil
}

// copyOf is a version of an event with its own fields, to be told anew.
func copyOf(e *events.Event) events.New {
	return events.New{UID: e.UID, ClassUID: ptrText(e.ClassUID), Course: e.CourseName(), Title: e.Title,
		Kind: e.Kind, Start: e.Start, End: e.EndText(), Room: e.Room, Required: e.Required, Cancelled: e.Cancelled}
}

// dayTwin is the event a new one tells again when one of the two has no time: the same type on the same day, of the
// same course or with the same title ("tomorrow the consultation is in S301" for the consultation known at 18:10).
func dayTwin(ev *events.Events, fresh events.New) (*events.Event, error) {
	k := schedule.TypeOf(fresh.Kind, false)
	onDay, err := ev.OnDay(pytext.Cut(fresh.Start, 10))
	if err != nil {
		return nil, err
	}
	for _, e := range onDay {
		if schedule.AllDay(e.Start) == schedule.AllDay(fresh.Start) || schedule.TypeOf(e.Kind, false) != k {
			continue // two timed ones on a day may well be two events
		}
		if (fresh.Course != "" && e.CourseName() == fresh.Course) || deadlines.SameLabel(e.Title, fresh.Title) {
			return e, nil
		}
	}
	return nil, nil
}

// join is what the new event adds to its twin, as a new version of it: the time of the timed one, the newer room.
func join(ev *events.Events, twin *events.Event, fresh events.New, settled map[int64]bool, now time.Time,
	changes *Changes) error {
	settled[twin.ID] = true
	if twin.Source == events.Own {
		return nil // the owner's: theirs to change
	}
	start, end := fresh.Start, fresh.End
	if schedule.AllDay(fresh.Start) {
		start, end = twin.Start, twin.EndText()
	}
	room := fresh.Room
	if room == "" {
		room = twin.Room
	}
	if start == twin.Start && end == twin.EndText() && room == twin.Room && !twin.Cancelled {
		return nil
	}
	version := events.New{UID: twin.UID, ClassUID: ptrText(twin.ClassUID), Course: twin.CourseName(),
		Title: twin.Title, Kind: twin.Kind, Required: twin.Required, Source: fresh.Source, ChatID: fresh.ChatID,
		MsgIDs: fresh.MsgIDs, Start: start, End: end, Room: room}
	newID, err := ev.Add(version, now)
	if err != nil {
		return err
	}
	if _, err := ev.Supersede(twin.ID, newID); err != nil {
		return err
	}
	changes.EventsMoved = append(changes.EventsMoved, [2]int64{twin.ID, newID})
	return nil
}

// retract withdraws the items all of whose messages were deleted; an event withdrawn this way brings back the
// version it had moved or cancelled. Superseded versions are withdrawn too, so a newer one withdrawn later never
// brings back a version told only by deleted messages; only those in force are news.
func (b *Batches) retract(chat *Box, changes *Changes) error {
	if chat.Source == "mail" {
		return nil // a letter is never deleted or edited
	}
	chatID, now := chat.ID, b.Now()
	since := now.Add(-RetractWindow)
	nw, err := b.News.InForce("tg", chatID, since)
	if err != nil {
		return err
	}
	dl, err := b.Deadlines.InForce("tg", chatID, since)
	if err != nil {
		return err
	}
	ev, err := b.Events.Versions("tg", chatID, since)
	if err != nil {
		return err
	}
	set := map[int64]bool{}
	for _, n := range nw {
		for _, i := range n.MsgIDs {
			set[i] = true
		}
	}
	for _, d := range dl {
		for _, i := range d.MsgIDs {
			set[i] = true
		}
	}
	for _, e := range ev {
		for _, i := range e.MsgIDs {
			set[i] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	msgIDs := slices.Sorted(maps.Keys(set))
	stored, err := b.Inbox.Messages(chatID, msgIDs, "tg")
	if err != nil {
		return err
	}
	deleted := func(ids []int64) bool {
		for _, i := range ids {
			if m := stored[i]; m == nil || m.DeletedAt == nil || *m.DeletedAt == "" {
				return false
			}
		}
		return true
	}
	var goneNews, goneDeadlines, goneEvents, goneInForce []int64
	for _, n := range nw {
		if deleted(n.MsgIDs) {
			goneNews = append(goneNews, n.ID)
		}
	}
	for _, d := range dl {
		if deleted(d.MsgIDs) {
			goneDeadlines = append(goneDeadlines, d.ID)
		}
	}
	for _, e := range ev { // by id: the oldest version first
		if deleted(e.MsgIDs) {
			goneEvents = append(goneEvents, e.ID)
			if e.ReplacedBy == nil {
				goneInForce = append(goneInForce, e.ID)
			}
		}
	}
	err = b.store.InTx(func(q *sql.Tx) error {
		if len(goneNews) > 0 {
			if err := b.News.With(q).Drop(goneNews, now); err != nil {
				return err
			}
		}
		if len(goneDeadlines) > 0 {
			if _, err := b.Deadlines.With(q).Drop(goneDeadlines, now); err != nil {
				return err
			}
		}
		if len(goneEvents) > 0 {
			if _, err := b.Events.With(q).Drop(goneEvents, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	changes.Gone = append(changes.Gone, goneDeadlines...)
	changes.EventsGone = append(changes.EventsGone, goneInForce...)
	return nil
}
