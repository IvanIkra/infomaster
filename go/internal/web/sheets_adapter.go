package web

import (
	"context"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	outsheets "github.com/IvanIkra/infomaster/go/internal/outputs/sheets"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
)

// Publisher is the sheets' publisher as the shelves read it: Parts.Sheets = Publisher{p, canUndo}. CanUndo tells
// whether an undo op is registered on the connector (the wiring passes one that asks the registry).
type Publisher struct {
	P       *outsheets.Publisher
	CanUndo func(op string) bool
}

func version(s *sheets.Sheet) *SheetVersion {
	if s == nil {
		return nil
	}
	return &SheetVersion{ID: s.ID, ChatID: s.ChatID, MsgID: s.MsgID, Name: s.Name, Sha256: s.Sha256,
		Version: s.Version, Status: s.Status, CreatedAt: s.CreatedAt, Changes: s.Changes}
}

// Listing is the sheets' listing.
func (p Publisher) Listing() ([]SheetListed, error) {
	rows, err := p.P.Sheets.Listing()
	if err != nil {
		return nil, err
	}
	out := make([]SheetListed, 0, len(rows))
	for _, r := range rows {
		out = append(out, SheetListed{ID: r.ID, First: r.First, Versions: r.Versions, Status: r.Status, Error: r.Error,
			Course: r.Course, Number: r.Number, Name: r.Name, Due: r.Due, DueAssumed: r.DueAssumed, Chat: r.Chat,
			Own: r.Own, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Summary: r.Summary, Caption: r.Caption,
			File: r.File})
	}
	return out, nil
}

func (p Publisher) sheet(v *SheetVersion) (*sheets.Sheet, error) { return p.P.Sheets.Get(v.ID) }

// Get is a sheet; nil for none.
func (p Publisher) Get(id int64) (*SheetVersion, error) {
	s, err := p.P.Sheets.Get(id)
	return version(s), err
}

// First is the first version of a sheet.
func (p Publisher) First(v *SheetVersion) (*SheetVersion, error) {
	s, err := p.sheet(v)
	if err != nil || s == nil {
		return v, err
	}
	first, err := p.P.Sheets.First(s)
	return version(first), err
}

// Versions are a sheet's versions, the first first.
func (p Publisher) Versions(first *SheetVersion) ([]*SheetVersion, error) {
	s, err := p.sheet(first)
	if err != nil || s == nil {
		return nil, err
	}
	chain, err := p.P.Sheets.Versions(s)
	out := make([]*SheetVersion, 0, len(chain))
	for _, c := range chain {
		out = append(out, version(c))
	}
	return out, err
}

// ByHash is the kept PDF's path, "" when it is gone.
func (p Publisher) ByHash(sha string) string { return p.P.Files.ByHash(sha) }

// Keep keeps an uploaded PDF.
func (p Publisher) Keep(_ context.Context, data []byte, name string, at time.Time) (KeptFile, error) {
	row, err := p.P.Files.Keep(data, name, at)
	if err != nil {
		return KeptFile{}, err
	}
	return KeptFile{Name: deref(row.Name), Row: row}, nil
}

// AddOwn is an own sheet of a kept PDF.
func (p Publisher) AddOwn(file KeptFile, course, number string, due *string, at time.Time) (int64, error) {
	row, _ := file.Row.(*files.Row)
	return p.P.Sheets.AddOwn(row, course, number, due, at, "")
}

// Publish publishes the sheets waiting.
func (p Publisher) Publish(ctx context.Context) error { return p.P.Publish(ctx) }

// CanRemoveOwn reports whether _remove_own_sheet is registered.
func (p Publisher) CanRemoveOwn() bool { return p.CanUndo != nil && p.CanUndo("_remove_own_sheet") }
