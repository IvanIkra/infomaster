package calc

import (
	"github.com/IvanIkra/infomaster/go/internal/pyjson"
)

// View is what a sheet's last read found (kv grades.view:<sid>, CONTRACT §6): the web app's gradebook screen reads
// it, and the calculator stands on it. The JSON is the Python service's; the field order is its key order.
type View struct {
	Course    string               `json:"course"`
	Tabs      []ViewTab            `json:"tabs"`
	Constants map[string]string    `json:"constants"`
	How       map[string]Explained `json:"how"`
}

// ViewTab is a tab as the web app shows it (processing/grades describe()).
type ViewTab struct {
	Tab        string            `json:"tab"`
	OwnRow     int               `json:"own_row"`
	NameCol    string            `json:"name_col"`
	Skip       bool              `json:"skip"`
	Reason     *string           `json:"reason"`
	By         string            `json:"by"` // you, haiku or failed
	HeaderRows []int             `json:"header_rows"`
	Columns    []Column          `json:"columns"`
	Works      []Work            `json:"works"`
	Formulas   map[string]string `json:"formulas"`
	Notes      map[string]string `json:"notes"`
}

// Column is one cell of the own row under its header.
type Column struct {
	Column string `json:"column"`
	Header string `json:"header"`
	Value  string `json:"value"`
}

// Work is a laid-out work over the own row: the points there, the grade on 0-10, and the share of the others
// graded in its column (nil when unknown).
type Work struct {
	Column      string        `json:"column"`
	Label       string        `json:"label"`
	Max         pyjson.Float  `json:"max"`
	BonusColumn *string       `json:"bonus_column"`
	Header      string        `json:"header"`
	Raw         *pyjson.Float `json:"raw"`
	Value       *pyjson.Float `json:"value"`
	Graded      *pyjson.Float `json:"graded"`
}

// Explained is how a formula counts, in a line for the grade card.
type Explained struct {
	En string `json:"en"`
	Ru string `json:"ru"`
}
