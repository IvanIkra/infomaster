package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const password = "app-password-123"

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func env(user, pass string) config.Env {
	return config.Env{MailUser: user, MailPassword: pass, MailHost: "imap.example.org", MailPort: 993}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// letter is a text/plain letter as a mail client sends it: the header words encoded, the body in 8 bits.
func letter(subject, body string, at time.Time) []byte {
	return []byte("From: =?utf-8?b?" + b64("Иванов Пётр") + "?= <ivanov@hse.ru>\r\n" +
		"Subject: =?utf-8?b?" + b64(subject) + "?=\r\n" +
		"Date: " + at.Format(time.RFC1123Z) + "\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + body + "\r\n")
}

// withPDF is a letter with a PDF attached as application/octet-stream.
func withPDF(at time.Time, pdf []byte) []byte {
	return []byte("From: =?utf-8?b?" + b64("Петров Иван") + "?= <petrov@hse.ru>\r\n" +
		"Subject: =?utf-8?b?" + b64("Листок 3") + "?=\r\nDate: " + at.Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"===b===\"\r\n\r\n" +
		"--===b===\r\nContent-Type: text/plain; charset=\"utf-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		"Добрый день! Новый листок во вложении.\r\n" +
		"--===b===\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"list3.pdf\"\r\n\r\n" +
		base64.StdEncoding.EncodeToString(pdf) + "\r\n--===b===--\r\n")
}

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, clock.TZ).UTC()

type fixture struct {
	mail  *Mail
	store *store.Store
	now   time.Time
	dir   string
}

func setup(t *testing.T, e config.Env) *fixture {
	t.Helper()
	dir := t.TempDir()
	s := must(store.Open(filepath.Join(dir, "x.db")))
	t.Cleanup(func() { s.Close() })
	f := &fixture{store: s, now: now, dir: dir}
	f.mail = New(e, s, must(settings.New(s)), inbox.Open(s), func() time.Time { return f.now })
	return f
}

func kv(t *testing.T, s *store.Store, key string) string {
	var value string
	if err := s.DB.QueryRow("select value from kv where key = ?", key).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestParseLikePython(t *testing.T) {
	var cases []struct {
		Raw          string   `json:"raw"`
		UID          int64    `json:"uid"`
		Sender       string   `json:"sender"`
		Address      string   `json:"address"`
		Subject      string   `json:"subject"`
		Date         string   `json:"date"`
		Text         string   `json:"text"`
		Files        []string `json:"files"`
		PDFName      *string  `json:"pdf_name"`
		PDFData      *string  `json:"pdf_data"`
		FieldsSender string   `json:"fields_sender"`
	}
	if err := json.Unmarshal(must(os.ReadFile("testdata/letters.json")), &cases); err != nil || len(cases) != 14 {
		t.Fatal(err)
	}
	fallback := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	for _, c := range cases {
		letter := Parse(must(base64.StdEncoding.DecodeString(c.Raw)), c.UID, fallback)
		date := must(time.Parse(time.RFC3339, c.Date))
		if letter.Sender != c.Sender || letter.Address != c.Address || letter.Subject != c.Subject ||
			letter.Text != c.Text || !letter.Date.Equal(date) || *letter.Fields().SenderName != c.FieldsSender {
			t.Errorf("letter %d:\n%q %q %q %v\n%q\nwant\n%q %q %q %v\n%q", c.UID, letter.Sender, letter.Address,
				letter.Subject, letter.Date, letter.Text, c.Sender, c.Address, c.Subject, date, c.Text)
		}
		if len(c.Files) == 0 {
			c.Files = nil
		}
		if !reflect.DeepEqual(letter.Files, c.Files) {
			t.Errorf("letter %d: files %q, want %q", c.UID, letter.Files, c.Files)
		}
		switch {
		case c.PDFName == nil && letter.PDF != nil:
			t.Errorf("letter %d: a PDF %q", c.UID, letter.PDF.Name)
		case c.PDFName != nil && (letter.PDF == nil || letter.PDF.Name != *c.PDFName ||
			base64.StdEncoding.EncodeToString(letter.PDF.Data) != *c.PDFData):
			t.Errorf("letter %d: no PDF %q", c.UID, *c.PDFName)
		}
	}
}

func TestParseCutsQuotesAndSignature(t *testing.T) {
	body := "Добрый день!\nКР переносится на 5 ноября.\n> старая цитата\n-- \nИванов П.\n" +
		"18.10.2026, 12:00, \"Студент\" <s@edu.hse.ru>:\nвопрос"
	parsed := Parse(letter("ДЗ 3 по алгебре", body, now), 7, time.Time{})
	if parsed.UID != 7 || parsed.Sender != "Иванов Пётр" || parsed.Subject != "ДЗ 3 по алгебре" ||
		parsed.Text != "Добрый день!\nКР переносится на 5 ноября." {
		t.Fatalf("%+v", parsed)
	}
	entry := FileEntry("task.pdf")
	if !reflect.DeepEqual(entry, inbox.File{"kind": "pdf", "name": "task.pdf", "mime": nil, "size": nil}) ||
		FileEntry("a.docx")["kind"] != "document" {
		t.Fatal(entry)
	}
}

func TestTextHelpers(t *testing.T) {
	if got := HTMLToText("<p>a<br>b<br/>c</p><div class='x yandex_quote'>q</div><img class=gmail_quote>rest"); got !=
		"\na\nb\nc\n" {
		t.Fatalf("%q", got)
	}
	for _, line := range []string{"On Mon, 19 Oct 2026 at 10:00 A. Person <a@b.c> wrote:", "Иван Иванов пишет:",
		"-----Original Message-----", "-- Исходное сообщение --", "__________", "20.10.2026, 10:00, \"Имя\" <a@b.ru>:",
		"\u00a0ON TUESDAY SOMEONE WROTE:"} {
		if !ReplyHeader.MatchString(line) {
			t.Errorf("not a reply header: %q", line)
		}
	}
	if ReplyHeader.MatchString("Write to me: a@b.ru") || ReplyHeader.MatchString("_________") {
		t.Error("a reply header")
	}
	if got := StripQuotes("a\r\n  > q\r\nb\r\n--  \r\nsig"); got != "a\nb" {
		t.Fatalf("%q", got)
	}
	if got := Tidy("\u200b x \u00a0 y \n\n\n\n z\u2028w "); got != "x y\n\nz\nw" {
		t.Fatalf("%q", got)
	}
}

func TestFirstPollTakesOnlyTheLastDay(t *testing.T) {
	f := setup(t, env("student@edu.hse.ru", password))
	fresh, old := now.Add(-2*time.Hour), now.Add(-3*24*time.Hour)
	saved := must(f.mail.Take(State{}, 555, 12, []Raw{{10, letter("ДЗ 3 по алгебре", "Сдать до пятницы.", fresh)},
		{11, letter("ДЗ 3 по алгебре", "Старое.", old)}, {12, []byte("\x00 broken")}}, true))
	if saved != 2 { // the broken one still makes a message: Parse never gives up on a letter
		t.Fatal(saved)
	}
	if got := kv(t, f.store, StateKey); got != `{"uidvalidity": 555, "last": 12, "base": 0}` {
		t.Fatal(got)
	}
	if m := must(f.mail.Inbox.Message(inbox.Mailbox, 10, "mail")); *m.Subject != "ДЗ 3 по алгебре" ||
		*m.SenderName != "Иванов Пётр <ivanov@hse.ru>" || m.SenderID != nil {
		t.Fatal(m)
	}
	if m := must(f.mail.Inbox.Message(inbox.Mailbox, 11, "mail")); m != nil {
		t.Fatal("an old letter is stored")
	}
	if m := must(f.mail.Inbox.Message(inbox.Mailbox, 12, "mail")); *m.SenderName != "?" || m.Text != "\x00 broken" {
		t.Fatal(m)
	}
}

func TestNewUIDValidityMovesPastEveryMsgID(t *testing.T) {
	f := setup(t, env("student@edu.hse.ru", password))
	must(f.mail.Take(State{}, 555, 12, []Raw{{12, letter("ДЗ 3 по алгебре", "x", now)}}, true))
	state := must(f.mail.state())
	must(f.mail.Take(state, 777, 3, []Raw{{3, letter("Новый ящик", "y", now)}}, true))
	if got := kv(t, f.store, StateKey); got != `{"uidvalidity": 777, "last": 3, "base": 12}` {
		t.Fatal(got)
	}
	if m := must(f.mail.Inbox.Message(inbox.Mailbox, 15, "mail")); *m.Subject != "Новый ящик" {
		t.Fatal(*m.Subject)
	}
	if m := must(f.mail.Inbox.Message(inbox.Mailbox, 12, "mail")); *m.Subject != "ДЗ 3 по алгебре" {
		t.Fatal(*m.Subject)
	}
}

func TestPollHidesThePasswordAndRecovers(t *testing.T) {
	f := setup(t, env("student@edu.hse.ru", password))
	ctx := context.Background()
	var told []i18n.Phrase
	f.mail.Notify = func(p i18n.Phrase) { told = append(told, p) }
	f.mail.Fetcher = func(context.Context, string, int, string, string, string, *int64, *int64, time.Time) (int64,
		int64, []Raw, error) {
		return 0, 0, nil, &net.OpError{Op: "dial", Err: errors.New("login failed for " + password)}
	}
	if n := must(f.mail.Poll(ctx)); n != 0 {
		t.Fatal(n)
	}
	status := must(f.mail.Status())
	if strings.Contains(*status.Error, password) || !strings.Contains(*status.Error, "***") {
		t.Fatal(*status.Error)
	}
	f.now = f.now.Add(4 * time.Hour)
	must(f.mail.Poll(ctx))
	if len(told) != 1 || strings.Contains(told[0].Ru, password) || !told[0].Alert ||
		!strings.HasPrefix(told[0].Ru, "Почта недоступна с 20.10 12:00: ") {
		t.Fatal(told)
	}

	f.mail.Fetcher = func(_ context.Context, _ string, _ int, _, _, _ string, _, _ *int64, since time.Time) (int64,
		int64, []Raw, error) {
		if since.Format(clock.DateLayout) != "2026-10-19" {
			t.Errorf("since %v", since)
		}
		return 555, 1, []Raw{{1, letter("ДЗ", "z", f.now)}}, nil
	}
	if n := must(f.mail.Poll(ctx)); n != 1 {
		t.Fatal(n)
	}
	status = must(f.mail.Status())
	if status.Error != nil || status.OKAt == nil || *status.Last != 1 || status.Letters != 1 || !status.Configured {
		t.Fatalf("%+v", status)
	}
	if told[len(told)-1].Ru != "Почта снова доступна." {
		t.Fatal(told)
	}
}

func TestNotConfigured(t *testing.T) {
	f := setup(t, env("", ""))
	if f.mail.Configured() || *must(f.mail.Status()).Error != NotSet {
		t.Fatal("configured")
	}
	if err := f.mail.Run(context.Background()); err != nil { // returns at once
		t.Fatal(err)
	}
}

func TestASheetInALetter(t *testing.T) {
	sheet := makePDF([]string{"Calculus. Sheet 3. Series\nHand in by 30.10 23:59 in the LMS", "Task 1."})
	if parsed := Parse(withPDF(now, sheet), 0, time.Time{}); parsed.PDF == nil || parsed.PDF.Name != "list3.pdf" ||
		!bytes.Equal(parsed.PDF.Data, sheet) {
		t.Fatal("no PDF")
	}
	f := setup(t, env("student@edu.hse.ru", password))
	f.mail.Files = files.New(f.store, f.dir)
	f.mail.Fetcher = func(context.Context, string, int, string, string, string, *int64, *int64, time.Time) (int64,
		int64, []Raw, error) {
		return 555, 7, []Raw{{7, withPDF(now.Add(-time.Hour), sheet)}}, nil
	}
	if n := must(f.mail.Poll(context.Background())); n != 1 {
		t.Fatal(n)
	}
	message := must(f.mail.Inbox.Message(inbox.Mailbox, 7, "mail"))
	if !reflect.DeepEqual(message.Files, []inbox.File{{"kind": "pdf", "name": "list3.pdf", "mime": nil, "size": nil}}) {
		t.Fatal(message.Files)
	}
	row := must(f.mail.Files.Fetch(context.Background(), message))
	if row == nil || row.Sha256 == nil || !strings.HasPrefix(*row.FirstPage, "Calculus. Sheet 3") ||
		*row.Name != "list3.pdf" {
		t.Fatal(row)
	}
}

// --- a fake IMAP server -------------------------------------------------------------------

func serve(t *testing.T, letters [][]byte) (*imapmemserver.User, string) {
	t.Helper()
	memory := imapmemserver.New()
	user := imapmemserver.NewUser("student@edu.hse.ru", password)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for _, raw := range letters {
		appendLetter(t, user, raw)
	}
	memory.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memory.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
		Logger:       quiet{},
	})
	listener := must(net.Listen("tcp", "127.0.0.1:0"))
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	dial := Dial
	Dial = func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
	t.Cleanup(func() { Dial = dial })
	return user, listener.Addr().String()
}

type quiet struct{}

func (quiet) Printf(string, ...any) {}

type literal struct{ *bytes.Reader }

func appendLetter(t *testing.T, user *imapmemserver.User, raw []byte) {
	if _, err := user.Append("INBOX", literal{bytes.NewReader(raw)}, &imap.AppendOptions{Time: now}); err != nil {
		t.Fatal(err)
	}
}

func TestFetchFromAServer(t *testing.T) {
	big := append(letter("Большое письмо", "", now), bytes.Repeat([]byte("x"), 3000)...)
	user, _ := serve(t, [][]byte{letter("Первое", "a", now), big, letter("Третье", "c", now)})
	ctx := context.Background()
	since := clock.LocalDay(now.Add(-24 * time.Hour))
	limit := LetterBytes
	LetterBytes = 2000
	t.Cleanup(func() { LetterBytes = limit })

	validity, top, letters, err := Fetch(ctx, "imap.example.org", 993, "student@edu.hse.ru", password, Folder, nil,
		nil, since)
	if err != nil {
		t.Fatal(err)
	}
	if validity != 1 || top != 3 || len(letters) != 3 {
		t.Fatal(validity, top, len(letters))
	}
	if Parse(letters[0].Data, 1, time.Time{}).Subject != "Первое" {
		t.Fatal(string(letters[0].Data))
	}
	header := Parse(letters[1].Data, 2, time.Time{}) // its headers only, and a line why
	if header.Subject != "Большое письмо" || header.Text != "(The letter is 0 MB: too big to read; open it in the mailbox.)" {
		t.Fatalf("%q", header.Text)
	}

	// The same UIDVALIDITY: only what is past the last UID; "n:*" matching the newest letter is not taken again.
	last := int64(3)
	_, top, letters, err = Fetch(ctx, "imap.example.org", 993, "student@edu.hse.ru", password, Folder, &validity,
		&last, since)
	if err != nil || top != 3 || len(letters) != 0 {
		t.Fatal(top, len(letters), err)
	}
	appendLetter(t, user, letter("Четвёртое", "d", now))
	_, top, letters, err = Fetch(ctx, "imap.example.org", 993, "student@edu.hse.ru", password, Folder, &validity,
		&last, since)
	if err != nil || top != 4 || len(letters) != 1 || letters[0].UID != 4 {
		t.Fatal(top, letters, err)
	}

	// The letters held at once are bounded: the state moves only past what was read.
	bound := PollBytes
	PollBytes = 10
	t.Cleanup(func() { PollBytes = bound })
	_, top, letters, err = Fetch(ctx, "imap.example.org", 993, "student@edu.hse.ru", password, Folder, nil, nil, since)
	if err != nil || top != 1 || len(letters) != 1 {
		t.Fatal(top, len(letters), err)
	}

	_, _, _, err = Fetch(ctx, "imap.example.org", 993, "student@edu.hse.ru", "wrong", Folder, nil, nil, since)
	if err == nil || errorName(err) != "error" {
		t.Fatal(err)
	}
}

func TestPollThroughAServer(t *testing.T) {
	serve(t, [][]byte{letter("Первое", "a", now.Add(-time.Hour)), letter("Старое", "b", now.Add(-72*time.Hour))})
	f := setup(t, env("student@edu.hse.ru", password))
	if n := must(f.mail.Poll(context.Background())); n != 1 { // the first poll: the last day only
		t.Fatal(n, *must(f.mail.Status()).Error)
	}
	if got := kv(t, f.store, StateKey); got != `{"uidvalidity": 1, "last": 2, "base": 0}` {
		t.Fatal(got)
	}
}

// makePDF is a PDF with one page per text (ASCII, one line each, Helvetica).
func makePDF(pages []string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var kids []string
	for _, text := range pages {
		var stream strings.Builder
		for i, line := range strings.Split(text, "\n") {
			fmt.Fprintf(&stream, "BT /F1 12 Tf 50 %d Td (%s) Tj ET\n", 750-16*i, line)
		}
		objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", stream.Len(), stream.String()))
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << "+
			"/Font << /F1 3 0 R >> >> /Contents %d 0 R >>", len(objects)))
		kids = append(kids, fmt.Sprintf("%d 0 R", len(objects)))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	out := []byte("%PDF-1.4\n")
	var offsets []int
	for n, body := range objects {
		offsets = append(offsets, len(out))
		out = append(out, fmt.Sprintf("%d 0 obj\n%s\nendobj\n", n+1, body)...)
	}
	xref := len(out)
	out = append(out, fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)...)
	for _, offset := range offsets {
		out = append(out, fmt.Sprintf("%010d 00000 n \n", offset)...)
	}
	return append(out, fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1,
		xref)...)
}
