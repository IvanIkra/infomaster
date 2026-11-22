// Package mail is the university mailbox over IMAP (§5.2, D6, D30): the port of Tablo's sources/mail.py (§6.2).
//
// Only the @edu.hse.ru box (Yandex 360), with an app password from .env (MAIL_USER, MAIL_PASSWORD; without them the
// source is off and says so). INBOX is opened read-only and letters are fetched with BODY.PEEK, so nothing is marked
// as read. A letter is new when its UID is above the last one seen (kv "mail.state"); the first poll, or one after
// UIDVALIDITY changed, takes only letters of the last `news_ttl_hours` (24 h). 60 s per socket operation; one poll
// every 5 minutes.
//
// A letter becomes a message (source "mail") of the mailbox's pseudo chat (inbox.Mailbox): the sender, the subject,
// the names of the attachments and the body without HTML, quoted replies and the signature, cleaned and cut at 8000
// characters. Its first PDF attachment is kept by Files before the letter is stored, so the batch reading the letter
// finds it and the model may see a sheet in it (D59). The batches parse it with the chats (processing/batches): every
// letter is study, and the mailbox comes after the study chats and before the others (D30). Its msg_id is the UID
// plus an offset that grows past every msg_id given before whenever UIDVALIDITY changes, so a rebuilt box never
// collides with letters kept from the old one. The letters and the state are saved in one transaction: a letter is
// stored once. One broken letter does not stop the others.
//
// The password never leaves this package: an error is told without it. The owner hears once when the box has
// failed for 3 hours, and once when it works again.
package mail

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	netmail "net/mail"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	StateKey   = "mail.state" // {"uidvalidity", "last": the highest UID seen, "base": msg_id = base + UID}
	Folder     = "INBOX"
	PollEvery  = 5 * time.Minute  // §6.2
	Timeout    = 60 * time.Second // each socket operation
	MaxBody    = 8000             // characters of a letter's text (§6.2)
	StaleAfter = 3 * time.Hour    // failing longer than this is told to the owner
	Hidden     = "***"
	NotSet     = "MAIL_USER and MAIL_PASSWORD are not set"
	TooBig     = "(The letter is %.0f MB: too big to read; open it in the mailbox.)"
)

// Memory bounds (D79); variables so tests can lower them.
var (
	LetterBytes int64 = 15 << 20 // a bigger letter is read by its headers only
	PollBytes   int64 = 40 << 20 // letters held at once; the rest wait for the next poll
)

// --- the letter ---------------------------------------------------------------------

// Attachment is a letter's first PDF attachment, kept by Files (D59).
type Attachment struct {
	Name string
	Data []byte
}

// Letter is a letter as the source reads it.
type Letter struct {
	UID     int64
	Sender  string    // display name, else the address
	Subject string    //
	Date    time.Time // aware
	Text    string
	Files   []string
	PDF     *Attachment
	Address string // the From address: a display name is anyone's to choose (D79)
}

// Fields is the message the batches read: the text cut at MaxBody, the attachments by name.
func (l Letter) Fields() inbox.Fields {
	name := l.Sender
	if l.Address != "" && l.Address != l.Sender {
		name = fmt.Sprintf("%s <%s>", l.Sender, l.Address)
	}
	if name == "" {
		name = "?"
	}
	entries := make([]inbox.File, 0, len(l.Files))
	for _, f := range l.Files {
		entries = append(entries, FileEntry(f))
	}
	return inbox.Fields{SenderName: &name, At: l.Date, Text: pytext.Cut(l.Text, MaxBody), Files: entries,
		Subject: l.Subject}
}

// FileEntry is an attachment as the chats' files are listed: only the name; the first PDF is kept by Files (D59).
func FileEntry(name string) inbox.File {
	kind := "document"
	if strings.HasSuffix(strings.ToLower(name), ".pdf") {
		kind = "pdf"
	}
	return inbox.File{"kind": kind, "name": name, "mime": nil, "size": nil}
}

func squeeze(s string) string { return strings.Join(pytext.Split(s), " ") }

var angle = regexp.MustCompile(`^\s*(.*?)\s*<([^<>]*)>\s*$`)

// from is the sender's display name and address of a From header.
func from(value string) (name, address string) {
	parser := netmail.AddressParser{WordDecoder: wordDecoder}
	if list, err := parser.ParseList(utf8Replace([]byte(value))); err == nil {
		if len(list) == 0 {
			return "", ""
		}
		// A name in a comment ("a@b.example (Name)") is no display name to Python: only "Name <a@b>" has one.
		if list[0].Name != "" && strings.Contains(value, "<") {
			return list[0].Name, list[0].Address
		}
		return list[0].Address, list[0].Address
	}
	// What the strict parser refuses, read by hand: "Name" <address>, or the address alone.
	decoded := decodeHeader(value)
	if m := angle.FindStringSubmatch(decoded); m != nil {
		name := strings.Trim(m[1], `"`)
		address := strings.TrimSpace(m[2])
		if name == "" {
			name = address
		}
		return name, address
	}
	return decoded, ""
}

// Parse reads a letter; it never refuses one: what cannot be read is left empty, a date that cannot be read is
// `fallback` (now when zero).
func Parse(raw []byte, uid int64, fallback time.Time) Letter {
	msg := parseMessage(raw, false)
	var sender, address string
	if value, ok := msg.get("From"); ok {
		sender, address = from(value)
	}
	date, err := time.Time{}, errors.New("no date")
	if value, ok := msg.get("Date"); ok {
		date, err = netmail.ParseDate(strings.TrimSpace(decodeHeader(value)))
	}
	if err != nil {
		date = fallback
		if date.IsZero() {
			date = clock.NowUTC()
		}
	}
	subject, _ := msg.get("Subject")
	var names []string
	var first *part
	for _, p := range msg.attachments() {
		name := p.filename()
		if name == "" {
			continue
		}
		names = append(names, name)
		if first == nil && (slices.Contains(files.PDFTypes, p.ctype) || strings.HasSuffix(strings.ToLower(name),
			".pdf")) {
			first = p
		}
	}
	letter := Letter{UID: uid, Sender: squeeze(sender), Subject: squeeze(decodeHeader(subject)), Date: date,
		Text: bodyText(msg), Files: names, Address: squeeze(address)}
	// Only bytes are an attachment's content: a text, a message or a multipart is left as a name.
	if first != nil && first.maintype() != "text" && first.maintype() != "multipart" && first.maintype() != "message" {
		if data := first.payload(); len(data) > 0 {
			letter.PDF = &Attachment{Name: first.filename(), Data: data}
		}
	}
	return letter
}

// --- IMAP -----------------------------------------------------------------------

// Raw is a letter as the server gives it.
type Raw struct {
	UID  int64
	Data []byte
}

// Dial connects to the server: TLS, Timeout to connect. Replaced in tests.
var Dial = func(ctx context.Context, address string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(address)
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: Timeout}, Config: &tls.Config{ServerName: host}}
	return dialer.DialContext(ctx, "tcp", address)
}

// timedConn gives every read and write Timeout.
type timedConn struct{ net.Conn }

func (c timedConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(Timeout))
	return c.Conn.Read(b)
}

func (c timedConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(Timeout))
	return c.Conn.Write(b)
}

// Fetcher is what a poll asks of the server; Fetch, replaced in tests.
type Fetcher func(ctx context.Context, host string, port int, user, password, folder string, uidvalidity,
	last *int64, since time.Time) (current, top int64, letters []Raw, err error)

// Fetch returns the folder's UIDVALIDITY, the highest UID in the folder, and the letters to read: those past `last`
// under the same UIDVALIDITY, else those since the day `since`.
func Fetch(ctx context.Context, host string, port int, user, password, folder string, uidvalidity, last *int64,
	since time.Time) (int64, int64, []Raw, error) {
	conn, err := Dial(ctx, net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return 0, 0, nil, err
	}
	client := imapclient.New(timedConn{conn}, nil)
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	if err := client.Login(user, password).Wait(); err != nil {
		return 0, 0, nil, err
	}
	selected, err := client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return 0, 0, nil, fmt.Errorf("cannot open folder %s: %w", folder, err)
	}
	current := int64(selected.UIDValidity)
	status, err := client.Status(folder, &imap.StatusOptions{UIDNext: true}).Wait()
	if err != nil {
		return 0, 0, nil, err
	}
	top := int64(status.UIDNext) - 1
	same := uidvalidity != nil && current == *uidvalidity && last != nil
	criteria := &imap.SearchCriteria{}
	if same {
		if top <= *last {
			return current, top, nil, nil
		}
		var set imap.UIDSet
		set.AddRange(imap.UID(*last+1), 0) // n:*
		criteria.UID = []imap.UIDSet{set}
	} else {
		criteria.Since = since
	}
	found, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return 0, 0, nil, err
	}
	var uids []int64
	for _, uid := range found.AllUIDs() {
		if !same || int64(uid) > *last { // "n:*" always matches the newest letter
			uids = append(uids, int64(uid))
		}
	}
	slices.Sort(uids)
	var letters []Raw
	var total int64
	for n, uid := range uids {
		if total > PollBytes { // the rest at the next poll: the state moves only past what was read
			return current, uids[n-1], letters, nil
		}
		size, err := letterSize(client, uid)
		if err != nil {
			return 0, 0, nil, err
		}
		var raw []byte
		if size > LetterBytes { // its headers only, and a line why: anyone may send a letter of any size
			section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
			header, err := fetchSection(client, uid, section)
			if err != nil {
				return 0, 0, nil, err
			}
			if len(header) > 0 {
				raw = append(trimCRLF(header), []byte("\r\n\r\n"+fmt.Sprintf(TooBig, float64(size)/(1<<20)))...)
			}
		} else {
			if raw, err = fetchSection(client, uid, &imap.FetchItemBodySection{Peek: true}); err != nil {
				return 0, 0, nil, err
			}
		}
		if len(raw) > 0 {
			letters = append(letters, Raw{uid, raw})
			total += int64(len(raw))
		}
	}
	for _, uid := range uids {
		top = max(top, uid)
	}
	_ = client.Logout().Wait()
	return current, top, letters, nil
}

func trimCRLF(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	return b
}

// letterSize is a letter's size by the server, before it is downloaded; 0 when the server does not say.
func letterSize(client *imapclient.Client, uid int64) (int64, error) {
	got, err := client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{RFC822Size: true}).Collect()
	if err != nil {
		return 0, err
	}
	for _, m := range got {
		return m.RFC822Size, nil
	}
	return 0, nil
}

func fetchSection(client *imapclient.Client, uid int64, section *imap.FetchItemBodySection) ([]byte, error) {
	got, err := client.Fetch(imap.UIDSetNum(imap.UID(uid)),
		&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, err
	}
	for _, m := range got {
		if data := m.FindBodySection(section); data != nil {
			return data, nil
		}
	}
	return nil, nil
}

// --- the source ---------------------------------------------------------------------

// State is kv "mail.state", in Python's key order.
type State struct {
	UIDValidity *int64 `json:"uidvalidity"`
	Last        *int64 `json:"last"`
	Base        int64  `json:"base"`
}

// Status is status(): never the password. Last: the highest UID seen; Letters: those stored (14 days).
type Status struct {
	Configured bool    `json:"configured"`
	OKAt       *string `json:"ok_at"`
	Error      *string `json:"error"`
	Last       *int64  `json:"last"`
	Letters    int64   `json:"letters"`
}

// Mail is the source.
type Mail struct {
	Host     string
	Port     int
	User     string
	password string
	store    *store.Store
	settings *settings.Settings
	Inbox    *inbox.Inbox
	Now      func() time.Time
	Fetcher  Fetcher
	// Notify is a line for the owner (Web Push).
	Notify func(text i18n.Phrase)
	// Files keeps a letter's PDF attachment when it comes (D59); nil: not kept.
	Files *files.Files

	mu           sync.Mutex
	okAt         string
	err          string
	failingSince time.Time
	told         bool
}

// New makes the source; now nil is clock.NowUTC.
func New(env config.Env, s *store.Store, st *settings.Settings, in *inbox.Inbox, now func() time.Time) *Mail {
	if now == nil {
		now = clock.NowUTC
	}
	m := &Mail{Host: env.MailHost, Port: env.MailPort, User: env.MailUser, password: env.MailPassword, store: s,
		settings: st, Inbox: in, Now: now, Fetcher: Fetch}
	if !m.Configured() {
		m.err = NotSet
	}
	return m
}

// Configured reports whether the user and the password are set.
func (m *Mail) Configured() bool { return m.User != "" && m.password != "" }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (m *Mail) state() (State, error) {
	var state State
	_, err := m.store.GetJSON(StateKey, &state)
	return state, err
}

// Status is the source's status: never the password.
func (m *Mail) Status() (Status, error) {
	state, err := m.state()
	if err != nil {
		return Status{}, err
	}
	letters, err := m.Inbox.Letters()
	if err != nil {
		return Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{Configured: m.Configured(), OKAt: optional(m.okAt), Error: optional(m.err), Last: state.Last,
		Letters: letters}, nil
}

// Run polls every 5 minutes until ctx is done; at once without the user and the password.
func (m *Mail) Run(ctx context.Context) error {
	if !m.Configured() {
		slog.Info(NotSet + ": no mail")
		return nil
	}
	for {
		if _, err := m.Poll(ctx); err != nil {
			slog.Error("mail poll failed", "err", err)
		}
		timer := time.NewTimer(PollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Poll is one look into the box; it returns how many letters were stored.
func (m *Mail) Poll(ctx context.Context) (int, error) {
	state, err := m.state()
	if err != nil {
		return 0, err
	}
	ttl, err := m.ttl()
	if err != nil {
		return 0, err
	}
	since := clock.LocalDay(m.Now().Add(-ttl))
	uidvalidity, top, letters, err := m.Fetcher(ctx, m.Host, m.Port, m.User, m.password, Folder, state.UIDValidity,
		state.Last, since)
	if err != nil {
		m.failed(m.hide(errorName(err) + ": " + err.Error()))
		return 0, nil
	}
	first := state.UIDValidity == nil || uidvalidity != *state.UIDValidity || state.Last == nil
	rows, err := m.Read(state, uidvalidity, letters, first)
	if err != nil {
		return 0, err
	}
	if m.Files != nil { // before the letters: a batch that reads a letter finds its PDF
		for _, row := range rows {
			if pdf := row.Letter.PDF; pdf != nil {
				if _, err := m.Files.KeepAttachment(row.MsgID, row.Letter.Date, pdf.Name, pdf.Data); err != nil {
					slog.Error(fmt.Sprintf("mail: the attachment of letter %d not kept", row.MsgID), "err", err)
				}
			}
		}
	}
	saved, err := m.Save(state, uidvalidity, top, rows)
	if err != nil {
		return 0, err
	}
	m.recovered()
	if saved > 0 {
		slog.Info(fmt.Sprintf("mail: %d new letters", saved))
	}
	return saved, nil
}

// errorName stands for Python's type(e).__name__ in the status: what kind of failure it was.
func errorName(err error) string {
	var netErr net.Error
	var imapErr *imap.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	case errors.As(err, &imapErr):
		return "error"
	}
	return "OSError"
}

// Row is a letter to store, by its msg_id.
type Row struct {
	MsgID  int64
	Letter Letter
}

// Take stores the letters, on a first poll only those of the last day, and the new state in the same transaction.
// It returns how many were stored.
func (m *Mail) Take(state State, uidvalidity, top int64, letters []Raw, first bool) (int, error) {
	rows, err := m.Read(state, uidvalidity, letters, first)
	if err != nil {
		return 0, err
	}
	return m.Save(state, uidvalidity, top, rows)
}

// Read is the letters to store, by msg_id: on a first poll only those of the last day.
func (m *Mail) Read(state State, uidvalidity int64, letters []Raw, first bool) ([]Row, error) {
	base, err := m.base(state, uidvalidity)
	if err != nil {
		return nil, err
	}
	ttl, err := m.ttl()
	if err != nil {
		return nil, err
	}
	now := m.Now()
	cutoff := now.Add(-ttl)
	var rows []Row
	for _, raw := range letters {
		letter, ok := safeParse(raw, now)
		if !ok {
			continue
		}
		if first && letter.Date.Before(cutoff) {
			continue
		}
		rows = append(rows, Row{base + raw.UID, letter})
	}
	return rows, nil
}

// safeParse is Parse; one broken letter must not block the rest.
func safeParse(raw Raw, now time.Time) (letter Letter, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("mail: cannot read letter %d: %v", raw.UID, r))
			ok = false
		}
	}()
	return Parse(raw.Data, raw.UID, now), true
}

// Save stores the letters and the new state in one transaction; how many were stored.
func (m *Mail) Save(state State, uidvalidity, top int64, rows []Row) (int, error) {
	base, err := m.base(state, uidvalidity) // before the transaction: the store has one connection
	if err != nil {
		return 0, err
	}
	value, err := pyjson.DumpsASCII(State{UIDValidity: &uidvalidity, Last: &top, Base: base})
	if err != nil {
		return 0, err
	}
	saved := 0
	err = m.store.InTx(func(tx *sql.Tx) error {
		letters := m.Inbox.With(tx)
		for _, row := range rows {
			added, err := letters.AddLetter(row.MsgID, row.Letter.Fields())
			if err != nil {
				return err
			}
			if added {
				saved++
			}
		}
		_, err := tx.Exec("insert into kv(key, value) values(?, ?) on conflict(key) do update set value = excluded.value",
			StateKey, value)
		return err
	})
	if err != nil {
		return 0, err
	}
	return saved, nil
}

// base is the offset of msg_ids under this UIDVALIDITY: kept while it stays, past every msg_id given when it
// changes (the UIDs of a rebuilt box start anew).
func (m *Mail) base(state State, uidvalidity int64) (int64, error) {
	if state.UIDValidity != nil && *state.UIDValidity == uidvalidity {
		return state.Base, nil
	}
	var given int64
	if state.UIDValidity != nil {
		given = state.Base
		if state.Last != nil {
			given += *state.Last
		}
	}
	newest, err := m.Inbox.NewestLetter()
	if err != nil {
		return 0, err
	}
	return max(given, newest), nil
}

func (m *Mail) ttl() (time.Duration, error) {
	hours, err := m.settings.Int("news_ttl_hours")
	return time.Duration(hours) * time.Hour, err
}

// hide is an error's text without the password (the user name is no secret).
func (m *Mail) hide(text string) string {
	if m.password == "" {
		return text
	}
	return strings.ReplaceAll(text, m.password, Hidden)
}

func (m *Mail) failed(reason string) {
	slog.Warn("mail poll failed: " + reason)
	now := m.Now()
	m.mu.Lock()
	m.err = reason
	if m.failingSince.IsZero() {
		m.failingSince = now
	}
	var text *i18n.Phrase
	if !m.told && now.Sub(m.failingSince) >= StaleAfter {
		m.told = true
		since := m.failingSince.In(clock.TZ).Format("02.01 15:04")
		p := i18n.AlertP(fmt.Sprintf("The mailbox is unavailable since %s: %s.\nNew letters are not read; the read "+
			"ones stay in the news and the deadlines.", since, reason),
			fmt.Sprintf("Почта недоступна с %s: %s.\nНовые письма не читаются; прочитанные остаются в новостях и "+
				"дедлайнах.", since, reason))
		text = &p
	}
	m.mu.Unlock()
	if text != nil && m.Notify != nil {
		m.Notify(*text)
	}
}

func (m *Mail) recovered() {
	m.mu.Lock()
	told := m.told
	m.okAt, m.err = clock.LocalISO(m.Now()), ""
	m.failingSince, m.told = time.Time{}, false
	m.mu.Unlock()
	if told && m.Notify != nil {
		m.Notify(i18n.P("The mailbox is available again.", "Почта снова доступна."))
	}
}
