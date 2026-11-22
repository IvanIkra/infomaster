// Package sheets publishes the problem sheets (§6.3, D14, D69): a read sheet joins the list the web app's shelves
// show. The port of outputs/sheets.py.
//
// After every batch run and RUZ poll the sheets waiting are prepared (read, compared with their older version, put in
// the library) and published, oldest first; one that fails stays waiting for the next run, one whose file is gone
// fails. Published is the row's status `posted`, a name kept from the forum's time (D69), so a newer version chains
// to the older one. The owner's joining of two sheets, changing a sheet and deleting an own one go through here too,
// under the same lock.
package sheets

import (
	"context"
	"log/slog"
	"sync"

	"github.com/IvanIkra/infomaster/go/internal/collectors/files"
	"github.com/IvanIkra/infomaster/go/internal/processing/sheets"
)

// Publisher publishes the sheets.
type Publisher struct {
	Sheets *sheets.Sheets
	Files  *files.Files
	// Prepare is the preparing of a sheet: Sheets.Prepare; tests replace it.
	Prepare func(ctx context.Context, sheetID int64) (*sheets.Sheet, error)

	lock sync.Mutex
}

// New is the publisher of the sheets.
func New(s *sheets.Sheets) *Publisher {
	return &Publisher{Sheets: s, Files: s.Files, Prepare: s.Prepare}
}

// Publish publishes the sheets waiting. Only a failure to read the waiting ones is returned: a sheet that fails
// waits for the next run.
func (p *Publisher) Publish(ctx context.Context) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	waiting, err := p.Sheets.Waiting()
	if err != nil {
		return err
	}
	for _, w := range waiting {
		sheet, err := p.Prepare(ctx, w.ID)
		if err == nil && sheet != nil {
			err = p.publish(sheet)
		}
		if err != nil {
			slog.Error("publishing the sheet failed, it waits for the next run", "sheet", w.ID, "err", err)
		}
	}
	return nil
}

// Join makes a sheet published as new a newer version of another. An *i18n.Error when it cannot be.
func (p *Publisher) Join(ctx context.Context, sheetID, previousID int64) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	_, err := p.Sheets.Join(ctx, sheetID, previousID)
	return err
}

// Edit is the owner's change of a sheet (Sheets.Edit); nil for no such sheet.
func (p *Publisher) Edit(sheetID int64, fields map[string]*string) (*sheets.Sheet, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.Sheets.Edit(sheetID, fields)
}

// Remove deletes an own sheet with every version. False when it is no own sheet.
func (p *Publisher) Remove(sheetID int64) (bool, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	chain, err := p.Sheets.RemoveOwn(sheetID)
	return chain != nil, err
}

func (p *Publisher) publish(sheet *sheets.Sheet) error {
	if p.Files.ByHash(sheet.Sha256) == "" {
		return p.Sheets.Failed(sheet.ID, p.Sheets.FileGone())
	}
	return p.Sheets.Posted(sheet.ID)
}
