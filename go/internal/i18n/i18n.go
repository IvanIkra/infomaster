// Package i18n is the language of what the owner reads (D41): English or Russian, English by default.
//
// Text made away from its output (a collector's line for the owner, the reason a login failed, an error kept for a
// status) is a Phrase with both languages; the output picks one when it shows it. String() of a Phrase is the
// English: the log and the API for Tablo. An Alert is a Phrase about a source that stopped working: it is pushed.
package i18n

import "errors"

// Languages are the languages of the app.
var Languages = []string{"en", "ru"}

// Phrase is a text in both languages.
type Phrase struct {
	En, Ru string
	Alert  bool // a failure of a source (it is unavailable, a login is needed): pushed (D63)
}

// P makes a Phrase.
func P(en, ru string) Phrase { return Phrase{En: en, Ru: ru} }

// AlertP makes an Alert.
func AlertP(en, ru string) Phrase { return Phrase{En: en, Ru: ru, Alert: true} }

func (p Phrase) String() string { return p.En }

// Get is the phrase in the language.
func (p Phrase) Get(lang string) string {
	if lang == "ru" {
		return p.Ru
	}
	return p.En
}

// MarshalText writes the English, as str() does in the Python service; outputs that know the language use Localize.
func (p Phrase) MarshalText() ([]byte, error) { return []byte(p.En), nil }

// Tr picks one of two texts by the language.
func Tr(lang, en, ru string) string {
	if lang == "ru" {
		return ru
	}
	return en
}

// Error is an error raised with a Phrase.
type Error struct{ Phrase Phrase }

func (e *Error) Error() string { return e.Phrase.En }

// Errorf wraps a Phrase as an error.
func Err(en, ru string) error { return &Error{Phrase: P(en, ru)} }

// Reason is the text of an error, in the language when it was raised with a Phrase.
func Reason(err error, lang string) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Phrase.Get(lang)
	}
	return err.Error()
}

// Localize walks a JSON-ready value (maps, slices) and writes every Phrase in the language: json_default of the
// Python service.
func Localize(value any, lang string) any {
	switch v := value.(type) {
	case Phrase:
		return v.Get(lang)
	case *Phrase:
		if v == nil {
			return nil
		}
		return v.Get(lang)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = Localize(item, lang)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Localize(item, lang)
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Localize(item, lang)
		}
		return out
	default:
		return value
	}
}
