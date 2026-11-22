// Package changes is the journal of what the connector changed (§8.5): every write tool's call with a line about it
// and the steps that undo it. The port of connector/changes.py.
//
// «Machine → Claude connector» in the web app lists it with **Undo**: Claude reads messages strangers wrote, and a
// change it was talked into must be easy to see and to take back. The web app's changes (§8.6) go through the same
// tools and into the same journal (`via` web), so the owner's taps can be undone the same way.
//
// An undo runs its steps through the same tools (connector/tools) at the time it is asked for. A step fails when what
// it acts on has changed since (a deadline that left its source, an event the chat withdrew): the error is kept and
// shown, and the change stays as it is.
package changes

import (
	"database/sql"
	"errors"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Schema is the table, verbatim from the Python service.
const Schema = `
create table if not exists connector_changes (  -- every write through the MCP connector (§8.5)
    id integer primary key,
    at text not null,           -- local time
    grant_id integer,           -- the connection it came through (connector_grants)
    tool text not null,
    arguments text not null,    -- JSON, as the client sent them
    summary text not null,      -- one line for the web app
    undo text,                  -- JSON [{tool, arguments}]; null when it cannot be undone
    note text not null default '',  -- why not, or what an undo does not bring back
    undone_at text,
    undo_error text,            -- the last failed undo
    via text not null default 'claude'  -- claude: through the connector; web: the owner in the web app (§8.6)
);
`

const (
	// Keep is the changes listed and kept.
	Keep = 200
	// ArgumentsChars is where the stored arguments are cut (code points).
	ArgumentsChars = 4000
)

// Via of a change: through the connector, or the owner in the web app.
const (
	ViaClaude = "claude"
	ViaWeb    = "web"
)

// Change is a row of the journal. Arguments are the decoded JSON, or {"cut": text} when the stored text was cut at
// ArgumentsChars; Undo is nil when the change cannot be undone.
type Change struct {
	ID        int64        `json:"id"`
	At        string       `json:"at"`
	GrantID   *int64       `json:"grant_id"`
	Tool      string       `json:"tool"`
	Arguments any          `json:"arguments"`
	Summary   string       `json:"summary"`
	Undo      []tools.Step `json:"undo"`
	Note      string       `json:"note"`
	UndoneAt  *string      `json:"undone_at"`
	UndoError *string      `json:"undo_error"`
	Via       string       `json:"via"`
}

// Changes is the journal.
type Changes struct {
	db  *sql.DB
	Now func() time.Time
}

// New makes the table (and its `via` column in a database of an earlier stage).
func New(s *store.Store, now func() time.Time) (*Changes, error) {
	if now == nil {
		now = clock.NowUTC
	}
	if _, err := s.DB.Exec(Schema); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query("pragma table_info(connector_changes)")
	if err != nil {
		return nil, err
	}
	found := false
	columns, _ := rows.Columns()
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			rows.Close()
			return nil, err
		}
		for i, column := range columns {
			if column == "name" && asString(values[i]) == "via" {
				found = true
			}
		}
	}
	rows.Close()
	if !found {
		if _, err := s.DB.Exec("alter table connector_changes add column via text not null default 'claude'"); err != nil {
			return nil, err
		}
	}
	return &Changes{db: s.DB, Now: now}, nil
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// Record journals a change; it returns its id. Only the newest Keep changes stay. via is ViaClaude or ViaWeb.
func (c *Changes) Record(grantID *int64, tool string, arguments map[string]any, summary string, undo []tools.Step,
	note, via string) (int64, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	text, err := pyjson.Dumps(httpx.PyValue(arguments))
	if err != nil {
		return 0, err
	}
	var undoText any
	if undo != nil {
		steps := make([]httpx.Object, len(undo))
		for i, step := range undo {
			arguments := step.Arguments
			if arguments == nil {
				arguments = map[string]any{}
			}
			steps[i] = httpx.Object{{Key: "tool", Value: step.Tool}, {Key: "arguments", Value: httpx.PyValue(arguments)}}
		}
		dumped, err := pyjson.Dumps(steps)
		if err != nil {
			return 0, err
		}
		undoText = dumped
	}
	tx, err := c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec("insert into connector_changes(at, grant_id, tool, arguments, summary, undo, note, via) "+
		"values(?, ?, ?, ?, ?, ?, ?, ?)", clock.LocalISO(c.Now()), grantID, tool, pytext.Cut(text, ArgumentsChars),
		summary, undoText, note, via)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("delete from connector_changes where id <= ?", id-Keep); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

const columns = "id, at, grant_id, tool, arguments, summary, undo, note, undone_at, undo_error, via"

func scan(row interface{ Scan(...any) error }) (*Change, error) {
	var change Change
	var grantID sql.NullInt64
	var arguments string
	var undo, undoneAt, undoError sql.NullString
	if err := row.Scan(&change.ID, &change.At, &grantID, &change.Tool, &arguments, &change.Summary, &undo,
		&change.Note, &undoneAt, &undoError, &change.Via); err != nil {
		return nil, err
	}
	if grantID.Valid {
		change.GrantID = &grantID.Int64
	}
	if undoneAt.Valid {
		change.UndoneAt = &undoneAt.String
	}
	if undoError.Valid {
		change.UndoError = &undoError.String
	}
	if decoded, err := httpx.Decode([]byte(arguments)); err == nil {
		change.Arguments = decoded
	} else { // cut at ArgumentsChars
		change.Arguments = map[string]any{"cut": arguments}
	}
	if undo.Valid {
		steps, err := decodeSteps(undo.String)
		if err != nil {
			return nil, err
		}
		change.Undo = steps
	}
	return &change, nil
}

// decodeSteps reads a stored undo: [{tool, arguments}], the arguments decoded as httpx.Decode does.
func decodeSteps(text string) ([]tools.Step, error) {
	decoded, err := httpx.Decode([]byte(text))
	if err != nil {
		return nil, err
	}
	list, ok := decoded.([]any)
	if !ok {
		return nil, errors.New("the undo of a change is not a list")
	}
	steps := make([]tools.Step, 0, len(list))
	for _, item := range list {
		object, _ := item.(map[string]any)
		name, _ := object["tool"].(string)
		arguments, _ := object["arguments"].(map[string]any)
		steps = append(steps, tools.Step{Tool: name, Arguments: arguments})
	}
	return steps, nil
}

// Get is a change, nil for none.
func (c *Changes) Get(id int64) (*Change, error) {
	change, err := scan(c.db.QueryRow("select "+columns+" from connector_changes where id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return change, err
}

// Recent are the newest changes first; only those made one way (via) when it is not "".
func (c *Changes) Recent(limit int, via string) ([]*Change, error) {
	var rows *sql.Rows
	var err error
	if via == "" {
		rows, err = c.db.Query("select "+columns+" from connector_changes order by id desc limit ?", limit)
	} else {
		rows, err = c.db.Query("select "+columns+" from connector_changes where via = ? order by id desc limit ?",
			via, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Change{}
	for rows.Next() {
		change, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, change)
	}
	return out, rows.Err()
}

// Undone marks a change undone (errText nil), or keeps the error of a failed undo.
func (c *Changes) Undone(id int64, errText *string) error {
	var err error
	if errText == nil {
		_, err = c.db.Exec("update connector_changes set undone_at = ?, undo_error = null where id = ?",
			clock.LocalISO(c.Now()), id)
	} else {
		_, err = c.db.Exec("update connector_changes set undo_error = ? where id = ?", *errText, id)
	}
	return err
}
