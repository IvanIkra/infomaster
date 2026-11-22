package grades

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	collector "github.com/IvanIkra/infomaster/go/internal/collectors/grades"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/processing/calc"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/settings"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Grades is the processing of the grade sheets.
type Grades struct {
	Store    *store.Store
	DB       *sql.DB
	Settings *settings.Settings
	LLM      llm.Model // nil: no layout, no explanations
	Now      func() time.Time
	Wake     func() // the collector polls at once after a change of the settings
}

// New makes the processing.
func New(st *store.Store, s *settings.Settings, model llm.Model) *Grades {
	return &Grades{Store: st, DB: st.DB, Settings: s, LLM: model, Now: clock.NowUTC}
}

func (g *Grades) wake() {
	if g.Wake != nil {
		g.Wake()
	}
}

func (g *Grades) sheetIDs() (map[string]bool, error) {
	sheets, err := g.Settings.GradeSheets()
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, s := range sheets {
		sid, err := collector.SheetID(s.URL)
		if err != nil {
			return nil, err
		}
		ids[sid] = true
	}
	return ids, nil
}

// --- a sheet read ---------------------------------------------------------------------------------------------

// Apply is GradeSheets.OnSheet: lay out every tab, then record the grades. An LLM error or a budget error when a tab
// could not be laid out: nothing is recorded then. A sheet the settings dropped while it was read, or read with a
// name the settings no longer have, records nothing either.
func (g *Grades) Apply(ctx context.Context, sheet collector.Sheet) (collector.Read, error) {
	var items []Item
	unread := []string{}
	tabs := []calc.ViewTab{}
	wanted := calc.Referenced(sheet.Views)
	for _, view := range sheet.Views {
		spec, err := g.Layout(ctx, sheet.ID, view, Budget(sheet.Course))
		if err != nil {
			return collector.Read{}, err
		}
		if spec.Failed {
			unread = append(unread, view.Tab)
		}
		items = append(items, Extract(sheet.ID, sheet.Course, view, spec, len(items))...)
		tabs = append(tabs, Describe(view, spec, wanted[view.Tab]))
	}
	current, err := g.current(sheet)
	if err != nil || !current {
		return collector.Read{Unread: []string{}}, err
	}
	kept := calc.View{Course: sheet.Course, Tabs: tabs, Constants: calc.Constants(sheet.Views)}
	kept.How = g.Explain(ctx, sheet.ID, kept)
	if err := g.Store.PutJSON(ViewKey+sheet.ID, kept); err != nil {
		return collector.Read{}, err
	}
	first, err := g.Record(sheet.ID, sheet.Title, items)
	if err != nil {
		return collector.Read{}, err
	}
	var n int
	if err := g.DB.QueryRow("select count(*) from grade_items where sheet_id = ? and not gone", sheet.ID).Scan(&n); err != nil {
		return collector.Read{}, err
	}
	return collector.Read{First: first, Items: n, Unread: unread}, nil
}

type explained struct {
	Hash string                    `json:"hash"`
	How  map[string]calc.Explained `json:"how"`
}

// Explain is how each formula of the own row counts, in a line for the grade card (17.6): "tab!COL" -> {en, ru}.
// One call for the sheet, cached while its formulas, written with names, stay the same (a student added above does
// not change them); none when the model cannot answer, and the card shows the formula with names then.
func (g *Grades) Explain(ctx context.Context, sid string, view calc.View) map[string]calc.Explained {
	written := calc.WrittenCells(calc.NewBook(view))
	how := map[string]calc.Explained{}
	if len(written) == 0 || g.LLM == nil {
		return how
	}
	cells := make([]string, 0, len(written))
	for c := range written {
		cells = append(cells, c)
	}
	sort.Strings(cells)
	if len(cells) > ExplainCells {
		cells = cells[:ExplainCells]
	}
	var lines []string
	for _, cell := range cells {
		tab, col := calc.Split(cell)
		header := ""
	find:
		for _, t := range view.Tabs {
			if t.Tab != tab {
				continue
			}
			for _, c := range t.Columns {
				if c.Column == col {
					header = c.Header
					break find
				}
			}
		}
		if header == "" {
			header = "-"
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s", cell, header, written[cell]))
	}
	prompt := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(strconv.Itoa(ExplainVersion) + prompt))
	digest := hex.EncodeToString(sum[:])[:24]
	var cached explained
	if ok, err := g.Store.GetJSON(ExplainKey+sid, &cached); err == nil && ok && cached.Hash == digest {
		if cached.How == nil {
			return how
		}
		return cached.How
	}
	raw, err := g.LLM.JSON(ctx, llm.Request{Task: "grades", System: ExplainSystem, Prompt: prompt,
		Schema: ExplainSchema, MaxTokens: max(1000, 120*len(cells))})
	var answer struct {
		Cells []struct {
			Cell string `json:"cell"`
			En   string `json:"en"`
			Ru   string `json:"ru"`
		} `json:"cells"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &answer)
	}
	if err != nil {
		slog.Info("the formulas of a sheet are not explained", "sheet", sid, "error", err)
		return how
	}
	for _, it := range answer.Cells {
		if _, ok := written[it.Cell]; ok && it.En != "" && it.Ru != "" {
			how[it.Cell] = calc.Explained{
				En: pytext.Cut(strings.Join(pytext.Split(it.En), " "), ExplainChars),
				Ru: pytext.Cut(strings.Join(pytext.Split(it.Ru), " "), ExplainChars)}
		}
	}
	if err := g.Store.PutJSON(ExplainKey+sid, explained{digest, how}); err != nil {
		slog.Warn("the explanations are not kept", "error", err)
	}
	return how
}

func (g *Grades) current(sheet collector.Sheet) (bool, error) {
	student, err := g.Settings.String("grades_student")
	if err != nil {
		return false, err
	}
	ids, err := g.sheetIDs()
	if err != nil {
		return false, err
	}
	return sheet.Student == student && ids[sheet.ID], nil
}

type cachedLayout struct {
	Watch  []int               `json:"watch"`
	Header []map[string]string `json:"header"`
	Spec   Spec                `json:"spec"`
}

// Layout is the owner's layout of the tab; else the cached one while the rows it watches have the same text, else
// Haiku's. The spec's header rows are rows of the view.
func (g *Grades) Layout(ctx context.Context, sid string, view collector.TabView, labelBudget int) (Spec, error) {
	var own Spec
	if ok, err := g.Store.GetJSON(OwnLayoutKey+sid+"|"+view.Tab, &own); err != nil || ok {
		return own, err
	}
	key := LayoutKey(sid, view.Tab, labelBudget)
	var cached cachedLayout
	ok, err := g.Store.GetJSON(key, &cached)
	if err != nil {
		return Spec{}, err
	}
	if ok && reflect.DeepEqual(view.Header(cached.Watch), cached.Header) {
		return cached.Spec, nil
	}
	system := strings.ReplaceAll(LayoutSystem, "{budget}", strconv.Itoa(labelBudget))
	var numbers []int
	columns := map[string]bool{}
	for c := range view.Mine {
		columns[c] = true
	}
	for _, r := range view.Rows {
		numbers = append(numbers, r.Number)
		for c := range r.Cells {
			columns[c] = true
		}
	}
	if g.LLM == nil {
		return Spec{}, llm.NewError(llm.Failed, "the LLM is unavailable")
	}
	var spec Spec
	raw, err := g.LLM.JSON(ctx, llm.Request{Task: "grades", System: system, Prompt: LayoutPrompt(view),
		Schema: LayoutSchema, MaxTokens: max(MaxTokens, TokensPerWork*len(columns))})
	if err == nil {
		if jerr := json.Unmarshal(raw, &spec); jerr != nil {
			err = llm.NewError(llm.BadAnswer, "the answer does not match the schema: "+jerr.Error())
		}
	}
	switch {
	case err == nil:
		var rows []int
		for _, row := range spec.HeaderRows {
			if slices.Contains(numbers, row) && !slices.Contains(rows, row) {
				rows = append(rows, row)
			}
		}
		slices.Sort(rows)
		spec.HeaderRows = rows
		if spec.HeaderRows == nil {
			spec.HeaderRows = []int{}
		}
		if spec.Items == nil {
			spec.Items = []LayoutItem{}
		}
	case llm.IsBadAnswer(err): // the same input would fail again: skipped until the top rows change
		slog.Warn("layout of a tab failed; the tab is skipped", "tab", view.Tab, "error", err)
		reason := "no answer: " + err.Error()
		spec = Spec{Skip: true, Reason: &reason, HeaderRows: []int{}, Items: []LayoutItem{}, Failed: true}
	default:
		return Spec{}, err
	}
	// The header the model named is what the layout stands on; with none named, the whole top.
	watch := spec.HeaderRows
	if len(watch) == 0 {
		watch = numbers
	}
	if watch == nil {
		watch = []int{}
	}
	if err := g.Store.PutJSON(key, cachedLayout{watch, view.Header(watch), spec}); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

type prevItem struct {
	key, tab, header string
	value            float64
	bonus            sql.NullFloat64
	gone             bool
}

type shiftKey struct {
	tab, header string
	value       float64
	bonus       float64
	hasBonus    bool
}

func bonusKey(b *float64) (float64, bool) {
	if b == nil {
		return 0, false
	}
	return *b, true
}

func nullable(b *float64) any {
	if b == nil {
		return nil
	}
	return *b
}

// Record stores the grades of a sheet and the new ones among them; true for the baseline (the first read).
func (g *Grades) Record(sid string, title *string, items []Item) (bool, error) {
	db := g.DB
	var one int
	err := db.QueryRow("select 1 from grade_sheets where sheet_id = ?", sid).Scan(&one)
	first := errors.Is(err, sql.ErrNoRows)
	if err != nil && !first {
		return false, err
	}
	rows, err := db.Query("select key, tab, header, value, bonus, gone from grade_items where sheet_id = ?", sid)
	if err != nil {
		return false, err
	}
	var order []string
	prev := map[string]prevItem{}
	for rows.Next() {
		var p prevItem
		if err := rows.Scan(&p.key, &p.tab, &p.header, &p.value, &p.bonus, &p.gone); err != nil {
			rows.Close()
			return false, err
		}
		prev[p.key] = p
		order = append(order, p.key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	keys := map[string]bool{}
	for _, it := range items {
		keys[it.Key] = true
	}
	// A column inserted before others shifts their letters and so their keys: a work that leaves one key in this
	// read and is found under another with the same tab, header and grade is the same work, not a new grade.
	shifted := map[shiftKey][]prevItem{}
	for _, k := range order {
		p := prev[k]
		if !keys[p.key] && !p.gone {
			sk := shiftKey{p.tab, p.header, p.value, p.bonus.Float64, p.bonus.Valid}
			shifted[sk] = append(shifted[sk], p)
		}
	}
	now := clock.LocalISO(g.Now())
	seen, moved := map[string]bool{}, map[string]bool{}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, it := range items {
		old, found := prev[it.Key]
		bonus, hasBonus := bonusKey(it.Bonus)
		sk := shiftKey{it.Tab, it.Header, it.Value, bonus, hasBonus}
		if same := shifted[sk]; !found && len(same) > 0 {
			old, found = same[len(same)-1], true
			shifted[sk] = same[:len(same)-1]
			moved[old.key] = true
			for _, table := range []string{"grade_events", "own_grades"} { // its history and the owner's change go along
				if _, err := tx.Exec("update "+table+" set key = ? where key = ?", it.Key, old.key); err != nil {
					return false, err
				}
			}
		}
		// A zero before any real grade is a placeholder for "not graded yet" (Tablo's §3.6), unless the work is
		// graded for the group (D72): then the zero is the student's own.
		if it.Value == 0 && it.Bonus == nil && (!found || old.value == 0) && (it.Graded == nil || *it.Graded < GradedShare) {
			continue
		}
		seen[it.Key] = true
		if _, err := tx.Exec("insert into grade_items(key, sheet_id, tab, header, label, value, raw, max, bonus, "+
			"position) values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?) on conflict(key) do update set label = excluded.label, "+
			"value = excluded.value, raw = excluded.raw, max = excluded.max, bonus = excluded.bonus, "+
			"position = excluded.position, gone = 0",
			it.Key, sid, it.Tab, it.Header, it.Label, it.Value, it.Raw, it.Max, nullable(it.Bonus), it.Position); err != nil {
			return false, err
		}
		oldBonus := 0.0
		if found && old.bonus.Valid {
			oldBonus = old.bonus.Float64
		}
		changed := !found || old.value != it.Value || oldBonus != bonus
		if changed { // the sheet changed the grade: the owner's change of it is over
			if _, err := tx.Exec("delete from own_grades where key = ?", it.Key); err != nil {
				return false, err
			}
		}
		if changed && !first {
			var previous any
			if found {
				previous = old.value
			}
			if _, err := tx.Exec("insert into grade_events(key, sheet_id, label, value, raw, max, bonus, previous, "+
				"seen_at) values(?, ?, ?, ?, ?, ?, ?, ?, ?)", it.Key, sid, it.Label, it.Value, it.Raw, it.Max,
				nullable(it.Bonus), previous, now); err != nil {
				return false, err
			}
		}
	}
	// A moved work lives under its new key now; the others that left may come back.
	for key := range moved {
		if _, err := tx.Exec("delete from grade_items where key = ?", key); err != nil {
			return false, err
		}
	}
	for _, key := range order {
		if !seen[key] && !moved[key] {
			if _, err := tx.Exec("update grade_items set gone = 1 where key = ?", key); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.Exec("insert into grade_sheets(sheet_id, title, baseline_at) values(?, ?, ?) "+
		"on conflict(sheet_id) do update set title = coalesce(excluded.title, title)", sid, title, now); err != nil {
		return false, err
	}
	return first, tx.Commit()
}

// --- the API, the status and the web app ----------------------------------------------------------------------

// NewGrade is one grade of the API's window (Tablo's §3.7).
type NewGrade struct {
	Key    string        `json:"key"`
	Label  string        `json:"label"`
	Value  pyjson.Float  `json:"value"`
	Bonus  *pyjson.Float `json:"bonus"`
	SeenAt string        `json:"seen_at"`
}

func pf(n sql.NullFloat64) *pyjson.Float {
	if !n.Valid {
		return nil
	}
	f := pyjson.Float(n.Float64)
	return &f
}

func (g *Grades) since() (string, error) {
	days, err := g.Settings.Int("grades_window_days")
	if err != nil {
		return "", err
	}
	return clock.LocalISO(g.Now().Add(-time.Duration(days) * 24 * time.Hour)), nil
}

type ownRow struct {
	id                   int64
	key, sheetID, course sql.NullString
	label, createdAt     string
	value                float64
	sheetValue           sql.NullFloat64
}

func (g *Grades) ownRows(where string, args ...any) ([]ownRow, error) {
	rows, err := g.DB.Query("select id, key, sheet_id, course, label, value, sheet_value, created_at from own_grades "+
		where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ownRow
	for rows.Next() {
		var r ownRow
		if err := rows.Scan(&r.id, &r.key, &r.sheetID, &r.course, &r.label, &r.value, &r.sheetValue, &r.createdAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (r ownRow) fullLabel() string {
	if !r.key.Valid && r.course.Valid && r.course.String != "" {
		return r.course.String + " " + r.label
	}
	return r.label
}

// New is the newest event of each work within the window, newest first (Tablo's §3.7); the owner's grades of the
// window among them, a changed one in place of the sheet's. A work no longer on its sheet is left out: a column a
// new layout dropped (a sum next to the grade of the same test) would stand as a second grade.
func (g *Grades) New() ([]NewGrade, error) {
	ids, err := g.sheetIDs()
	if err != nil {
		return nil, err
	}
	since, err := g.since()
	if err != nil {
		return nil, err
	}
	rows, err := g.DB.Query("select key, sheet_id, label, value, bonus, seen_at from grade_events where id in "+
		"(select max(id) from grade_events where seen_at >= ? group by key) and key in "+
		"(select key from grade_items where not gone) order by seen_at desc, id desc", since)
	if err != nil {
		return nil, err
	}
	var events []NewGrade
	for rows.Next() {
		var e NewGrade
		var sheetID string
		var value float64
		var bonus sql.NullFloat64
		if err := rows.Scan(&e.Key, &sheetID, &e.Label, &value, &bonus, &e.SeenAt); err != nil {
			rows.Close()
			return nil, err
		}
		if ids[sheetID] {
			e.Value, e.Bonus = pyjson.Float(value), pf(bonus)
			events = append(events, e)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	all, err := g.ownRows("where created_at >= ?", since)
	if err != nil {
		return nil, err
	}
	var own []ownRow
	changed := map[string]bool{}
	for _, r := range all {
		if !r.sheetID.Valid || ids[r.sheetID.String] {
			own = append(own, r)
			if r.key.Valid {
				changed[r.key.String] = true
			}
		}
	}
	listed := []NewGrade{}
	for _, e := range events {
		if !changed[e.Key] {
			listed = append(listed, e)
		}
	}
	for _, r := range own {
		key := r.key.String
		if !r.key.Valid {
			key = fmt.Sprintf("own-%d", r.id)
		}
		listed = append(listed, NewGrade{key, r.fullLabel(), pyjson.Float(r.value), nil, r.createdAt})
	}
	sort.SliceStable(listed, func(i, j int) bool { return listed[i].SeenAt > listed[j].SeenAt })
	return listed, nil
}

// SheetItem is one grade of a sheet as the web app shows it.
type SheetItem struct {
	ID         string        `json:"id"`
	Label      string        `json:"label"`
	Value      pyjson.Float  `json:"value"`
	Raw        pyjson.Float  `json:"raw"`
	Max        pyjson.Float  `json:"max"`
	Bonus      *pyjson.Float `json:"bonus"`
	Edited     bool          `json:"edited"`
	SheetValue pyjson.Float  `json:"sheet_value"`
	New        bool          `json:"new"`
}

// Items is the grades of a sheet in sheet order, the new ones marked, the owner's changes in place (ID: the work's
// key).
func (g *Grades) Items(sid string) ([]SheetItem, error) {
	since, err := g.since()
	if err != nil {
		return nil, err
	}
	fresh := map[string]bool{}
	rows, err := g.DB.Query("select distinct key from grade_events where sheet_id = ? and seen_at >= ?", sid, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, err
		}
		fresh[key] = true
	}
	rows.Close()
	own, err := g.ownRows("where sheet_id = ?", sid)
	if err != nil {
		return nil, err
	}
	changed := map[string]ownRow{}
	for _, r := range own {
		changed[r.key.String] = r
	}
	rows, err = g.DB.Query("select key, label, value, raw, max, bonus from grade_items where sheet_id = ? and not gone "+
		"order by position", sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	listed := []SheetItem{}
	for rows.Next() {
		var key, label string
		var value, raw, maximum float64
		var bonus sql.NullFloat64
		if err := rows.Scan(&key, &label, &value, &raw, &maximum, &bonus); err != nil {
			return nil, err
		}
		item := SheetItem{ID: key, Label: label, Value: pyjson.Float(value), Raw: pyjson.Float(raw),
			Max: pyjson.Float(maximum), Bonus: pf(bonus), SheetValue: pyjson.Float(value), New: fresh[key]}
		if mine, ok := changed[key]; ok {
			item.Label, item.Value, item.Edited = mine.label, pyjson.Float(mine.value), true
			item.New = item.New || mine.createdAt >= since
		}
		listed = append(listed, item)
	}
	return listed, rows.Err()
}

// --- the owner's grades (D44) -------------------------------------------------------------------------------------

// ManualGrade is a grade added by hand.
type ManualGrade struct {
	ID     string       `json:"id"`
	Course *string      `json:"course"`
	Label  string       `json:"label"`
	Value  pyjson.Float `json:"value"`
	New    bool         `json:"new"`
}

// Manual is the grades added by hand, in the order they came (ID: own-<n>).
func (g *Grades) Manual() ([]ManualGrade, error) {
	since, err := g.since()
	if err != nil {
		return nil, err
	}
	rows, err := g.ownRows("where key is null order by id")
	if err != nil {
		return nil, err
	}
	listed := []ManualGrade{}
	for _, r := range rows {
		var course *string
		if r.course.Valid {
			course = &r.course.String
		}
		listed = append(listed, ManualGrade{fmt.Sprintf("own-%d", r.id), course, r.label, pyjson.Float(r.value),
			r.createdAt >= since})
	}
	return listed, nil
}

// AddGrade adds a grade by hand; its row id.
func (g *Grades) AddGrade(course, label string, value float64) (int64, error) {
	res, err := g.DB.Exec("insert into own_grades(course, label, value, created_at) values(?, ?, ?, ?)",
		course, label, value, clock.LocalISO(g.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func ownID(gradeID string) (int64, bool) {
	rest, ok := strings.CutPrefix(gradeID, "own-")
	if !ok || rest == "" || strings.ContainsFunc(rest, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil
}

// ChangeGrade changes a grade added by hand (own-<n>), or a sheet's (by its work's key) as the owner's. False for no
// such grade. An empty label or course keeps the one there.
func (g *Grades) ChangeGrade(gradeID, label string, value float64, course string) (bool, error) {
	now := clock.LocalISO(g.Now())
	if id, ok := ownID(gradeID); ok {
		var rowCourse sql.NullString
		var rowLabel string
		err := g.DB.QueryRow("select course, label from own_grades where id = ? and key is null", id).
			Scan(&rowCourse, &rowLabel)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var newCourse any = rowCourse
		if course != "" {
			newCourse = course
		}
		if label == "" {
			label = rowLabel
		}
		_, err = g.DB.Exec("update own_grades set course = ?, label = ?, value = ?, created_at = ? where id = ?",
			newCourse, label, value, now, id)
		return err == nil, err
	}
	var sheetID, itemLabel string
	var sheetValue float64
	err := g.DB.QueryRow("select sheet_id, label, value from grade_items where key = ? and not gone", gradeID).
		Scan(&sheetID, &itemLabel, &sheetValue)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if label == "" {
		label = itemLabel
	}
	_, err = g.DB.Exec("insert into own_grades(key, sheet_id, label, value, sheet_value, created_at) "+
		"values(?, ?, ?, ?, ?, ?) on conflict(key) do update set label = excluded.label, value = excluded.value, "+
		"created_at = excluded.created_at", gradeID, sheetID, label, value, sheetValue, now)
	return err == nil, err
}

// DropGrade deletes a grade added by hand, or undoes the owner's change of a sheet's grade.
func (g *Grades) DropGrade(gradeID string) (bool, error) {
	var res sql.Result
	var err error
	if id, ok := ownID(gradeID); ok {
		res, err = g.DB.Exec("delete from own_grades where id = ? and key is null", id)
	} else {
		res, err = g.DB.Exec("delete from own_grades where key = ?", gradeID)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- the owner's view and layout of a sheet (D44) ----------------------------------------------------------------

// View is what the last read found in each tab: {course, tabs}; no tabs before the first read.
func (g *Grades) View(sid string) (calc.View, error) {
	var view calc.View
	ok, err := g.Store.GetJSON(ViewKey+sid, &view)
	if err != nil || !ok {
		return calc.View{Course: "", Tabs: []calc.ViewTab{}}, err
	}
	if view.Tabs == nil {
		view.Tabs = []calc.ViewTab{}
	}
	return view, nil
}

func findTab(view *calc.View, tab string) *calc.ViewTab {
	for i := range view.Tabs {
		if view.Tabs[i].Tab == tab {
			return &view.Tabs[i]
		}
	}
	return nil
}

// SetLayout is the owner's layout of a tab (items: column, label, max, bonus_column), over Haiku's from now on; the
// grades follow at the next read, which comes at once. The tab as the view has it now; nil for a tab the last read
// did not find. An *i18n.Error for a column the own row does not have, or a label too long.
func (g *Grades) SetLayout(sid, tab string, skip bool, items []LayoutItem) (*calc.ViewTab, error) {
	view, err := g.View(sid)
	if err != nil {
		return nil, err
	}
	found := findTab(&view, tab)
	if found == nil {
		return nil, nil
	}
	cells := map[string]bool{}
	for _, c := range found.Columns {
		cells[c.Column] = true
	}
	limit := Budget(view.Course)
	for _, it := range items {
		if !cells[it.Column] || (it.BonusColumn != nil && !cells[*it.BonusColumn]) {
			return nil, i18n.Err("no such column in the own row", "в своей строке нет такой колонки")
		}
		if it.Label == "" || pytext.Len(it.Label) > limit {
			return nil, i18n.Err(fmt.Sprintf("a label is needed, up to %d characters", limit),
				fmt.Sprintf("нужно название, до %d символов", limit))
		}
	}
	if items == nil {
		items = []LayoutItem{}
	}
	reason := "laid out by hand"
	headerRows := found.HeaderRows
	if headerRows == nil {
		headerRows = []int{}
	}
	spec := Spec{Skip: skip, Reason: &reason, HeaderRows: headerRows, Items: items, By: "you"}
	if err := g.Store.PutJSON(OwnLayoutKey+sid+"|"+tab, spec); err != nil {
		return nil, err
	}
	found.Skip, found.Reason, found.By, found.Works = skip, &reason, "you", Works(items, found.Columns, nil)
	if err := g.Store.PutJSON(ViewKey+sid, view); err != nil {
		return nil, err
	}
	g.wake()
	out := *found
	return &out, nil
}

// ClearLayout gives the tab back to Haiku: its layout as Haiku left it applies again. The tab as the view has it
// now.
func (g *Grades) ClearLayout(sid, tab string) (*calc.ViewTab, error) {
	view, err := g.View(sid)
	if err != nil {
		return nil, err
	}
	found := findTab(&view, tab)
	if found == nil {
		return nil, nil
	}
	if err := g.Store.DeleteJSON(OwnLayoutKey + sid + "|" + tab); err != nil {
		return nil, err
	}
	var cached cachedLayout
	ok, err := g.Store.GetJSON(LayoutKey(sid, tab, Budget(view.Course)), &cached)
	if err != nil {
		return nil, err
	}
	spec := Spec{}
	if ok {
		spec = cached.Spec
	}
	found.Skip, found.Reason = spec.Skip, spec.Reason
	found.By = "haiku"
	if spec.Failed {
		found.By = "failed"
	}
	found.Works = Works(spec.Items, found.Columns, nil)
	if err := g.Store.PutJSON(ViewKey+sid, view); err != nil {
		return nil, err
	}
	g.wake()
	out := *found
	return &out, nil
}

// Status is the processing's part of the grades status.
type Status struct {
	LLM bool `json:"llm"`
	New int  `json:"new"`
}

// Status says whether the model is there and how many grades are new.
func (g *Grades) Status() (Status, error) {
	listed, err := g.New()
	return Status{LLM: g.LLM != nil && g.LLM.Available(), New: len(listed)}, err
}

// --- the owner's settings ---------------------------------------------------------------------------------------

// Configure sets some of grades_student, grades_sheets, grades_window_days, all checked before any is saved. A
// sheet removed, or every sheet when the name changes, loses its grades: the next read is a baseline. A settings
// error when a value is wrong.
func (g *Grades) Configure(values map[string]any) error {
	if len(values) == 0 {
		return settings.NewError("expected some of " + strings.Join(SettingNames, ", "))
	}
	for name := range values {
		if !slices.Contains(SettingNames, name) {
			return settings.NewError("expected some of " + strings.Join(SettingNames, ", "))
		}
	}
	checked := map[string]any{}
	for _, name := range SettingNames {
		if value, ok := values[name]; ok {
			v, err := settings.Check(name, value)
			if err != nil {
				return err
			}
			checked[name] = v
		}
	}
	before, err := g.sheetIDs()
	if err != nil {
		return err
	}
	student, err := g.Settings.String("grades_student")
	if err != nil {
		return err
	}
	renamed := false
	if name, ok := checked["grades_student"]; ok && name != student {
		renamed = true
	}
	for _, name := range SettingNames {
		if value, ok := checked[name]; ok {
			if _, err := g.Settings.Set(name, value); err != nil {
				return err
			}
		}
	}
	after, err := g.sheetIDs()
	if err != nil {
		return err
	}
	var drop []string
	for sid := range before {
		if renamed || !after[sid] {
			drop = append(drop, sid)
		}
	}
	if renamed {
		for sid := range after {
			if !before[sid] {
				drop = append(drop, sid)
			}
		}
	}
	if err := g.Forget(drop); err != nil {
		return err
	}
	g.wake()
	return nil
}

// Forget drops a sheet's grades, the owner's changes of them, its view and the owner's layouts of it.
func (g *Grades) Forget(sids []string) error {
	tx, err := g.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sid := range sids {
		for _, table := range []string{"grade_items", "grade_events", "grade_sheets", "own_grades"} {
			if _, err := tx.Exec("delete from "+table+" where sheet_id = ?", sid); err != nil {
				return err
			}
		}
		prefix := OwnLayoutKey + sid + "|" // by the start of the key, not LIKE: an id may have "_" in it
		if _, err := tx.Exec("delete from kv where key in (?, ?) or substr(key, 1, ?) = ?", ViewKey+sid,
			ExplainKey+sid, pytext.Len(prefix), prefix); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(sids) > 0 {
		slog.Info("grades of sheets forgotten", "sheets", len(sids))
	}
	return nil
}
