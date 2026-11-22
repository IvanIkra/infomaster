// Package api is the internal API for Tablo (§8.1, §8.2): its own port, never behind Funnel, a Bearer token on /api/.
// The port of outputs/api.py.
//
// GET /api/deadlines: {"items": [{uid, title, due, category, source, overdue, assumed}]}, the merged list in screen
// order, as Tablo's route gave it (§8); `assumed` marks a sheet's date assumed by the service (§6.3). Own and hidden
// deadlines are kept in the web app and through Claude: HA's to-do lists are gone (D34), and with them the routes that
// changed the list.
//
// GET /api/news: the live news (D9, D23), the items of the web app's news and the same rules, in rank order:
// {"items": [{id, source, study, posted_at, full, short, importance}], "ttl_hours"}. Replaced, withdrawn and hidden
// items are not given. This is the level Б of §11.4: Tablo only lays them out in its column (§6.6).
//
// GET /api/grades: the new grades of the window (§6.7), {"items": [{key, label, value, bonus, seen_at}],
// "window_days"}, newest first: the newest event of each work. Tablo had no route for them (§8): this is the level В
// of §11.5.
//
// GET /api/status: every source's status and the LLM spending of today; an error kept as a Phrase is in English.
//
// The iCal feeds live here too, under /calendar/<secret>/ without the token (see icalfeed).
package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/llm"
	"github.com/IvanIkra/infomaster/go/internal/outputs/icalfeed"
	"github.com/IvanIkra/infomaster/go/internal/processing/grades"
	"github.com/IvanIkra/infomaster/go/internal/processing/merge"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/settings"
)

// Source is one source of /api/status: its name and its status (ok_at, error and its own flags), any value that
// writes a JSON object. The order of the sources is the order of the answer's keys.
type Source struct {
	Name   string
	Status func() (any, error)
}

// Budget is the LLM budget's status: *llm.Budget.
type Budget interface {
	Status() (llm.Status, error)
}

// Deadlines is the merged list: *merge.DeadlineList.
type Deadlines interface {
	Items() ([]merge.Item, error)
}

// API is the internal port's app (create_app). Grades, and News with Settings, are optional: their routes are there
// only when they are set.
type API struct {
	Env       config.Env // APIToken guards /api/; CalendarSecret turns the feeds on
	Budget    Budget
	Sources   []Source
	Calendar  *icalfeed.CalendarFeed // may be nil when Env.CalendarSecret is ""
	Deadlines Deadlines
	Grades    *grades.Grades // nil: no /api/grades
	News      *news.News     // with Settings: /api/news
	Settings  *settings.Settings
	Now       func() time.Time
}

// NewsFields are the fields of a news item given to Tablo.
var NewsFields = []string{"id", "source", "study", "posted_at", "full", "short", "importance"}

// New is the app with its required parts; set Grades, News and Settings after it when they are wired.
func New(env config.Env, budget Budget, sources []Source, calendar *icalfeed.CalendarFeed,
	deadlines Deadlines) *API {
	return &API{Env: env, Budget: budget, Sources: sources, Calendar: calendar, Deadlines: deadlines,
		Now: clock.NowUTC}
}

// Handler is the internal port: the routes behind the Bearer check of /api/.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	if a.Grades != nil {
		mux.Handle("GET /api/grades", httpx.Handle(a.grades))
	}
	if a.News != nil && a.Settings != nil {
		mux.Handle("GET /api/news", httpx.Handle(a.news))
	}
	mux.Handle("GET /api/status", httpx.Handle(a.status))
	mux.Handle("GET /api/deadlines", httpx.Handle(a.deadlines))
	if a.Env.CalendarSecret != "" && a.Calendar != nil {
		icalfeed.Routes(mux, a.Env.CalendarSecret, a.Calendar, nil)
	}
	return a.bearer(httpx.Router(mux))
}

// bearer guards every path under /api/, also one no route takes: "Bearer" exactly, then the token.
func (a *API) bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
			if scheme != "Bearer" || subtle.ConstantTimeCompare([]byte(token), []byte(a.Env.APIToken)) != 1 {
				httpx.Text(w, http.StatusUnauthorized, "Bearer token required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// status: every source that has joined so far, and the LLM spending of today. An error kept as a Phrase (D41) is
// given in English.
func (a *API) status(w http.ResponseWriter, _ *http.Request) error {
	body := httpx.Object{}
	for _, source := range a.Sources {
		value, err := source.Status()
		if err != nil {
			return err
		}
		body = append(body, httpx.Field{Key: source.Name, Value: value})
	}
	budget, err := a.Budget.Status()
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, append(body, httpx.Field{Key: "llm", Value: budget}))
	return nil
}

func (a *API) deadlines(w http.ResponseWriter, _ *http.Request) error {
	items, err := a.Deadlines.Items()
	if err != nil {
		return err
	}
	if items == nil {
		items = []merge.Item{}
	}
	httpx.JSON(w, http.StatusOK, httpx.Object{{Key: "items", Value: items}})
	return nil
}

func (a *API) news(w http.ResponseWriter, _ *http.Request) error {
	ttlHours, err := a.Settings.Int("news_ttl_hours")
	if err != nil {
		return err
	}
	items, err := a.News.Live(a.Now(), ttlHours)
	if err != nil {
		return err
	}
	listed := []httpx.Object{}
	for _, item := range items {
		listed = append(listed, httpx.Object{{Key: "id", Value: item.ID}, {Key: "source", Value: item.Source},
			{Key: "study", Value: item.Study}, {Key: "posted_at", Value: item.PostedAt},
			{Key: "full", Value: item.Full}, {Key: "short", Value: item.Short},
			{Key: "importance", Value: item.Importance}})
	}
	httpx.JSON(w, http.StatusOK, httpx.Object{{Key: "items", Value: listed}, {Key: "ttl_hours", Value: ttlHours}})
	return nil
}

func (a *API) grades(w http.ResponseWriter, _ *http.Request) error {
	items, err := a.Grades.New()
	if err != nil {
		return err
	}
	if items == nil {
		items = []grades.NewGrade{}
	}
	window, err := a.Grades.Settings.Get("grades_window_days")
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.Object{{Key: "items", Value: items}, {Key: "window_days", Value: window}})
	return nil
}
