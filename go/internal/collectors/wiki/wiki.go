// Package wiki is the course wikis (§5.2, D60): a course page of a MediaWiki (the faculty's wiki) as a source.
//
// Some courses post their sheets and homework only on the course's wiki page: a list of links to PDFs on Google
// Drive, and a table of due dates by group. The pages are listed in the `wiki_pages` setting. Every PollEvery the
// page's wikitext is read through the wiki's API (only when its revision changed); the links in it, outside HTML
// comments, are its items, each with its section and, in a table, the row's cells under the table's column heads.
//
// An item new on the page, or one whose row changed (a due date was set), becomes a notice in the mailbox: a letter
// from "Course wiki" whose subject names the page and the item and whose text gives the link and the row. The
// batches read it as they read a letter (D30): the PDF behind the link is fetched (collectors/links, D59), the model
// decides whether it is a sheet, and takes the due date of the student's group (it knows the group from `about_me`,
// D52). So the group is never configured here, and nothing new is needed downstream. A notice's msg_id is negative,
// counting down, so it never meets a letter's UID.
//
// Only links to files are items (links.Downloadable: Drive, Dropbox, Yandex Disk, PDFs): not the teachers'
// Telegram, not other pages, not the course's grade spreadsheet. The first read of a page is a baseline: of what is
// there already, only the last item of each section of tasks (sheets, homework) becomes a notice, not the whole
// semester and not the literature.
package wiki

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/collectors/inbox"
	"github.com/IvanIkra/infomaster/go/internal/collectors/links"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	pubnet "github.com/IvanIkra/infomaster/go/internal/net"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

const (
	StateKey   = "wiki.state" // {page url: {"revid", "title", "items": {link: row}}}
	NextKey    = "wiki.next"  // the msg_id of the next notice: negative, counting down
	PollEvery  = 3 * time.Hour
	Timeout    = 60 * time.Second
	Sender     = "Course wiki"
	StaleAfter = 24 * time.Hour // a page failing longer than this is told to the owner
)

// ws is Python's \s for text: Go's \s plus \v and the Unicode spaces.
const ws = `\s\x0b\x1c-\x1f\x85\xa0\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	link     = regexp.MustCompile(`\[(https?://[^` + ws + `\]]+)[` + ws + `]*([^\]]*)\]`)
	comment  = regexp.MustCompile(`(?s)<!--.*?(-->|$)`)
	markup   = regexp.MustCompile(`'{2,}|<[^>]+>`)
	wikiLink = regexp.MustCompile(`\[\[(?:[^\]|]*\|)?([^\]]*)\]\]`)
	cellSep  = regexp.MustCompile(`\|\||!!`)
	// tasks is a section or item of tasks: sheets, homework, tests (the baseline tells only these). Python's \b is
	// Unicode-aware, Go's is not: the word edges are spelled out.
	tasks = regexp.MustCompile(`(?i)листо?к|задач|задани|(?:^|[^\p{L}\p{N}_])дз(?:$|[^\p{L}\p{N}_])|домашн|` +
		`контрольн|sheet|problem|homework|assignment|(?:^|[^\p{L}\p{N}_])hw(?:$|[^\p{L}\p{N}_])`)
)

// Item is a link of a page.
type Item struct {
	URL     string
	Name    string
	Section string
	Row     string // a table's row: "101: 15.11 · 102: — · …" under the table's column heads
}

// valueError is Python's ValueError: a wrong address, an error the wiki answers.
type valueError struct{ msg string }

func (e *valueError) Error() string { return e.msg }

// WikiDown is the wiki answering with an error of its own (a database down): it passes, the page is read on a later
// poll.
type WikiDown struct{ Code string }

func (e *WikiDown) Error() string { return e.Code }

// repr is Python's repr() of a text.
func repr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	if quote == "'" {
		s = strings.ReplaceAll(s, "'", `\'`)
	}
	return quote + s + quote
}

// PageAPI is the api.php address and the page title of a wiki page's URL: /Title, /index.php?title=Title or
// /wiki/Title.
func PageAPI(url string) (api, title string, err error) {
	parts := links.SplitURL(pytext.Strip(url))
	if parts.Scheme != "http" && parts.Scheme != "https" || parts.Netloc == "" {
		return "", "", &valueError{"not a wiki page's address: " + repr(url)}
	}
	for _, pair := range links.ParseQSL(parts.Query, false) {
		if pair.Key == "title" {
			title = pair.Value
			break
		}
	}
	if title == "" {
		title = strings.TrimLeft(strings.TrimPrefix(links.Unquote(parts.Path), "/wiki/"), "/")
	}
	if title == "" || title == "index.php" || title == "api.php" {
		return "", "", &valueError{"no page title in " + repr(url)}
	}
	base := ""
	if strings.HasPrefix(parts.Path, "/w/") {
		base = "/w"
	}
	return "https://" + parts.Netloc + base + "/api.php", strings.ReplaceAll(title, "_", " "), nil
}

// Document is a link to a file that may be a sheet: Drive, Dropbox, Yandex Disk, a PDF. Not a chat, a person, a page,
// a spreadsheet (the course's grades).
func Document(url string) bool {
	return links.Downloadable(url) && !strings.Contains(url, "/spreadsheets/")
}

func squeeze(s string) string { return strings.Join(pytext.Split(s), " ") }

// Clean takes wiki markup out of a cell: links as their text, bold and tags gone, spaces squeezed.
func Clean(text string) string {
	text = link.ReplaceAllStringFunc(text, func(found string) string {
		m := link.FindStringSubmatch(found)
		if m[2] != "" {
			return m[2]
		}
		return m[1]
	})
	text = wikiLink.ReplaceAllString(text, "$1")
	return squeeze(markup.ReplaceAllString(text, ""))
}

// cells are the cells of a table line ("| a || b" or "! a !! b"), each without its attributes ("colspan=2 | x").
func cells(line string) []string {
	_, size := utf8.DecodeRuneInString(line)
	var found []string
	for _, cell := range cellSep.Split(line[size:], -1) {
		if head, rest, ok := strings.Cut(cell, "|"); ok && !strings.Contains(head, "[") && strings.Contains(head, "=") {
			cell = rest
		}
		found = append(found, pytext.Strip(cell))
	}
	return found
}

// heading is a heading line's text ("== Title ==", two to six '='); ok false for another line.
func heading(line string) (string, bool) {
	lead := len(line) - len(strings.TrimLeft(line, "="))
	for k := min(lead, 6); k >= 2; k-- {
		if len(line) >= 2*k && strings.HasSuffix(line, strings.Repeat("=", k)) {
			return pytext.Strip(line[k : len(line)-k]), true
		}
	}
	return "", false
}

// Items are the links of a page outside comments, in order, each once, with their section and table row.
func Items(wikitext string) []Item {
	text := comment.ReplaceAllString(wikitext, "")
	var found []Item
	seen := map[string]bool{}
	section := ""
	var heads, row []string

	closeRow := func() {
		current := row
		row = nil
		if len(current) == 0 {
			return
		}
		type pair struct{ url, name string }
		var docs []pair
		for _, cell := range current {
			for _, m := range link.FindAllStringSubmatch(cell, -1) {
				if Document(m[1]) {
					name := Clean(m[2])
					if name == "" {
						name = m[1]
					}
					docs = append(docs, pair{m[1], name})
				}
			}
		}
		if len(docs) == 0 {
			for _, cell := range current {
				if Clean(cell) != "" { // a row of column heads (the groups)
					heads = make([]string, len(current))
					for i, c := range current {
						heads[i] = Clean(c)
					}
					break
				}
			}
			return
		}
		url, name := docs[0].url, docs[0].name
		first := -1
		for i, cell := range current {
			if strings.Contains(cell, url) {
				first = i
				break
			}
		}
		var pairs []string
		for i, cell := range current {
			if i == first {
				continue
			}
			head := fmt.Sprintf("column %d", i+1)
			if i < len(heads) && heads[i] != "" {
				head = heads[i]
			}
			value := Clean(cell)
			if value == "" {
				value = "—"
			}
			pairs = append(pairs, head+": "+value)
		}
		if !seen[url] {
			seen[url] = true
			found = append(found, Item{url, name, section, strings.Join(pairs, " · ")})
		}
	}

	inTable := false
	for _, raw := range splitlines(text) {
		line := pytext.Strip(raw)
		if title, ok := heading(line); ok {
			section = Clean(title)
			continue
		}
		if strings.HasPrefix(line, "{|") {
			inTable, heads, row = true, nil, nil
			continue
		}
		if inTable {
			switch {
			case strings.HasPrefix(line, "|}"):
				closeRow()
				inTable = false
			case strings.HasPrefix(line, "|-"):
				closeRow()
			case strings.HasPrefix(line, "|"), strings.HasPrefix(line, "!"):
				row = append(row, cells(line)...)
			}
			continue
		}
		for _, m := range link.FindAllStringSubmatch(line, -1) {
			if url := m[1]; Document(url) && !seen[url] {
				name := Clean(m[2])
				if name == "" {
					name = url
				}
				seen[url] = true
				found = append(found, Item{URL: url, Name: name, Section: section})
			}
		}
	}
	return found
}

// Notice is the subject and the text of the letter that tells an item.
func Notice(title string, item Item, changed bool) (subject, text string) {
	subject = fmt.Sprintf("Wiki · %s: %s", title, item.Name)
	what := "New"
	if changed {
		what = "Changed"
	}
	section := item.Section
	if section == "" {
		section = "no section"
	}
	lines := []string{fmt.Sprintf("%s on the course's wiki page «%s», section «%s»: %s", what, title, section,
		item.Name), item.URL}
	if item.Row != "" {
		lines = append(lines, "Row of the table: "+item.Row)
	}
	return subject, strings.Join(lines, "\n")
}

// --- the stored state ------------------------------------------------------------------

// Ordered is a JSON object whose keys keep their order, as a Python dict does.
type Ordered[V any] struct {
	Keys   []string
	Values map[string]V
}

// Get is a key's value.
func (o Ordered[V]) Get(key string) (V, bool) {
	v, ok := o.Values[key]
	return v, ok
}

// Set puts a value: a new key goes last, a known one keeps its place.
func (o *Ordered[V]) Set(key string, value V) {
	if o.Values == nil {
		o.Values = map[string]V{}
	}
	if _, ok := o.Values[key]; !ok {
		o.Keys = append(o.Keys, key)
	}
	o.Values[key] = value
}

// Len is the number of keys.
func (o Ordered[V]) Len() int { return len(o.Keys) }

func marshal(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalJSON writes the keys in their order.
func (o Ordered[V]) MarshalJSON() ([]byte, error) {
	out := []byte("{")
	for i, key := range o.Keys {
		if i > 0 {
			out = append(out, ',')
		}
		k, err := marshal(key)
		if err != nil {
			return nil, err
		}
		v, err := marshal(o.Values[key])
		if err != nil {
			return nil, err
		}
		out = append(append(append(out, k...), ':'), v...)
	}
	return append(out, '}'), nil
}

// UnmarshalJSON reads the keys in their order.
func (o *Ordered[V]) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return fmt.Errorf("not a JSON object: %s", data)
	}
	*o = Ordered[V]{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		var value V
		if err := dec.Decode(&value); err != nil {
			return err
		}
		o.Set(t.(string), value)
	}
	_, err := dec.Token()
	return err
}

// Page is a page's entry of the state: its revision, its title and its items' rows by link.
type Page struct {
	Revid int64           `json:"revid"`
	Title string          `json:"title"`
	Items Ordered[string] `json:"items"`
}

// State is kv "wiki.state": the pages by URL.
type State = Ordered[Page]

// --- the source ---------------------------------------------------------------------

// Reader is (revision id, title, wikitext) of a page by the wiki's API.
type Reader func(ctx context.Context, api, title string) (int64, string, string, error)

// PageStatus is a listed page's status.
type PageStatus struct {
	Title *string `json:"title"`
	Items int     `json:"items"`
	Error *string `json:"error"`
}

// Status is status(): the listed pages, in the setting's order, and the last poll.
type Status struct {
	Pages Ordered[PageStatus] `json:"pages"`
	OKAt  *string             `json:"ok_at"`
}

// Wiki is the source.
type Wiki struct {
	store    *store.Store
	settings *settings.Settings
	Inbox    *inbox.Inbox
	Now      func() time.Time
	Reader   Reader // ReadPage; replaced in tests
	Notify   func(text i18n.Phrase)

	mu      sync.Mutex
	errors  map[string]string
	okAt    string
	failing map[string]time.Time
	told    map[string]bool
}

// New makes the source; now nil is clock.NowUTC.
func New(s *store.Store, st *settings.Settings, in *inbox.Inbox, now func() time.Time) *Wiki {
	if now == nil {
		now = clock.NowUTC
	}
	return &Wiki{store: s, settings: st, Inbox: in, Now: now, Reader: ReadPage, errors: map[string]string{},
		failing: map[string]time.Time{}, told: map[string]bool{}}
}

func (w *Wiki) state() (State, error) {
	var state State
	_, err := w.store.GetJSON(StateKey, &state)
	return state, err
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Status is the pages' status for the web app.
func (w *Wiki) Status() (Status, error) {
	state, err := w.state()
	if err != nil {
		return Status{}, err
	}
	urls, err := w.settings.Strings("wiki_pages")
	if err != nil {
		return Status{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var status Status
	for _, url := range urls {
		page := PageStatus{}
		if p, ok := state.Get(url); ok {
			page.Title, page.Items = &p.Title, p.Items.Len()
		}
		if e, ok := w.errors[url]; ok {
			page.Error = &e
		}
		status.Pages.Set(url, page)
	}
	status.OKAt = optional(w.okAt)
	return status, nil
}

// Run polls every 3 hours until ctx is done.
func (w *Wiki) Run(ctx context.Context) error {
	for {
		if _, err := w.Poll(ctx); err != nil {
			slog.Error("wiki poll failed", "err", err)
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

// errorName stands for Python's type(e).__name__: what kind of failure it was.
func errorName(err error) string {
	var netErr net.Error
	var value *valueError
	var status *statusError
	var key *keyError
	var syntax *json.SyntaxError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "TimeoutError"
	case errors.As(err, &value):
		return "ValueError"
	case errors.As(err, &status):
		return "ClientResponseError"
	case errors.As(err, &key):
		return "KeyError"
	case errors.As(err, &syntax):
		return "JSONDecodeError"
	}
	return "ClientError"
}

// Poll reads every listed page once; how many notices were stored.
func (w *Wiki) Poll(ctx context.Context) (int, error) {
	state, err := w.state()
	if err != nil {
		return 0, err
	}
	urls, err := w.settings.Strings("wiki_pages")
	if err != nil {
		return 0, err
	}
	stored := 0
	for _, url := range urls {
		api, title, err := PageAPI(url)
		var revid int64
		var wikitext string
		if err == nil {
			revid, title, wikitext, err = w.Reader(ctx, api, title)
		}
		var down *WikiDown
		if errors.As(err, &down) {
			w.failed(url, fmt.Sprintf("the wiki's server failed (%s)", down.Code))
			continue
		}
		if err != nil {
			w.failed(url, errorName(err)+": "+err.Error())
			continue
		}
		w.recovered(url)
		if page, ok := state.Get(url); ok && page.Revid == revid {
			continue
		}
		n, err := w.take(url, &state, revid, title, Items(wikitext))
		if err != nil {
			return stored, err
		}
		stored += n
	}
	w.mu.Lock()
	w.okAt = clock.LocalISO(w.Now())
	w.mu.Unlock()
	return stored, nil
}

type told struct {
	item    Item
	changed bool
}

// take stores the notices of a page's new and changed items and its new state, in one transaction.
func (w *Wiki) take(url string, state *State, revid int64, title string, found []Item) (int, error) {
	page, known := state.Get(url)
	var tell []told
	if !known { // a baseline: the current sheet and homework, the last item of their sections
		last := map[string]int{}
		for i, item := range found {
			if tasks.MatchString(item.Section + " " + item.Name) {
				last[item.Section] = i
			}
		}
		for i, item := range found {
			if j, ok := last[item.Section]; ok && j == i {
				tell = append(tell, told{item, false})
			}
		}
	} else {
		for _, item := range found {
			if row, ok := page.Items.Get(item.URL); !ok || row != item.Row {
				tell = append(tell, told{item, ok})
			}
		}
	}
	var items Ordered[string]
	for _, item := range found {
		items.Set(item.URL, item.Row)
	}
	state.Set(url, Page{Revid: revid, Title: title, Items: items})
	nextID := int64(-1)
	if _, err := w.store.GetJSON(NextKey, &nextID); err != nil { // before the transaction: one connection
		return 0, err
	}
	stateText, err := pyjson.Dumps(state)
	if err != nil {
		return 0, err
	}
	now := w.Now()
	sender := Sender
	stored := 0
	err = w.store.InTx(func(tx *sql.Tx) error {
		letters := w.Inbox.With(tx)
		for _, t := range tell {
			subject, text := Notice(title, t.item, t.changed)
			added, err := letters.AddLetter(nextID, inbox.Fields{SenderName: &sender, At: now, Text: text,
				Files: []inbox.File{}, Subject: subject})
			if err != nil {
				return err
			}
			if added {
				stored++
			}
			nextID--
		}
		nextText, err := pyjson.Dumps(nextID)
		if err != nil {
			return err
		}
		for _, kv := range [][2]string{{StateKey, stateText}, {NextKey, nextText}} {
			if _, err := tx.Exec("insert into kv(key, value) values(?, ?) on conflict(key) do update set "+
				"value = excluded.value", kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if stored > 0 {
		slog.Info(fmt.Sprintf("wiki %s: %d notices", title, stored))
	}
	return stored, nil
}

func (w *Wiki) failed(url, reason string) {
	slog.Warn(fmt.Sprintf("wiki page %s not read: %s", url, reason))
	now := w.Now()
	w.mu.Lock()
	w.errors[url] = reason
	since, ok := w.failing[url]
	if !ok {
		since = now
		w.failing[url] = since
	}
	var text *i18n.Phrase
	if !w.told[url] && now.Sub(since) >= StaleAfter && w.Notify != nil {
		w.told[url] = true
		at := since.In(clock.TZ).Format("02.01 15:04")
		p := i18n.AlertP(fmt.Sprintf("The course wiki page %s is not read since %s: %s.", url, at, reason),
			fmt.Sprintf("Страница вики курса %s не читается с %s: %s.", url, at, reason))
		text = &p
	}
	w.mu.Unlock()
	if text != nil {
		w.Notify(*text)
	}
}

func (w *Wiki) recovered(url string) {
	w.mu.Lock()
	wasTold := w.told[url]
	delete(w.errors, url)
	delete(w.failing, url)
	delete(w.told, url)
	w.mu.Unlock()
	if wasTold && w.Notify != nil {
		w.Notify(i18n.P(fmt.Sprintf("The course wiki page %s is read again.", url),
			fmt.Sprintf("Страница вики курса %s снова читается.", url)))
	}
}

// --- the wiki's API ---------------------------------------------------------------------

// NewClient is the HTTP client of the wiki: public addresses only (D79). Replaced in tests.
var NewClient = func() *http.Client { return pubnet.Client(Timeout) }

type statusError struct {
	code   int
	reason string
	url    string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%d, message=%s, url=%s", e.code, repr(e.reason), repr(e.url))
}

type keyError struct{ key string }

func (e *keyError) Error() string { return repr(e.key) }

// ReadPage is (revision id, title, wikitext) of a page by the MediaWiki API. The wiki refuses requests without a user
// agent of their own (curl's is refused).
func ReadPage(ctx context.Context, api, title string) (int64, string, string, error) {
	query := links.URLEncode([]links.Pair{{Key: "action", Value: "parse"}, {Key: "page", Value: title},
		{Key: "prop", Value: "wikitext|revid"}, {Key: "format", Value: "json"}, {Key: "formatversion", Value: "2"},
		{Key: "redirects", Value: "1"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"?"+query, nil)
	if err != nil {
		return 0, "", "", err
	}
	req.Header.Set("User-Agent", "infomaster")
	resp, err := NewClient().Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, "", "", &statusError{resp.StatusCode, http.StatusText(resp.StatusCode), req.URL.String()}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, "", "", err
	}
	var data struct {
		Error *struct {
			Code any    `json:"code"`
			Info string `json:"info"`
		} `json:"error"`
		Parse *struct {
			Revid    *json.Number `json:"revid"`
			Title    *string      `json:"title"`
			Wikitext *string      `json:"wikitext"`
		} `json:"parse"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return 0, "", "", err
	}
	if data.Error != nil {
		code := ""
		if data.Error.Code != nil && data.Error.Code != "" {
			code = fmt.Sprint(data.Error.Code)
		}
		if rest, ok := strings.CutPrefix(code, "internal_api_error"); ok { // the wiki's own failure: not the page's
			return 0, "", "", &WikiDown{strings.TrimPrefix(rest, "_")}
		}
		if data.Error.Info != "" {
			return 0, "", "", &valueError{data.Error.Info}
		}
		return 0, "", "", &valueError{code}
	}
	switch {
	case data.Parse == nil:
		return 0, "", "", &keyError{"parse"}
	case data.Parse.Revid == nil:
		return 0, "", "", &keyError{"revid"}
	case data.Parse.Title == nil:
		return 0, "", "", &keyError{"title"}
	case data.Parse.Wikitext == nil:
		return 0, "", "", &keyError{"wikitext"}
	}
	revid, err := data.Parse.Revid.Int64()
	if err != nil {
		return 0, "", "", &valueError{err.Error()}
	}
	return revid, *data.Parse.Title, *data.Parse.Wikitext, nil
}

// splitlines is str.splitlines(): lines split at \n, \r, \r\n, \v, \f, \x1c-\x1e, \x85, U+2028 and U+2029.
func splitlines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			start = i + size
		case '\r':
			out = append(out, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				size = 2
			}
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
