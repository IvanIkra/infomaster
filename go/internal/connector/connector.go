// Package connector is the MCP connector (§8.5, D57): infomaster as a remote MCP server that claude.ai adds as a
// custom connector. The port of connector/__init__.py.
//
// On the public port, next to the web app:
//   - /mcp: the endpoint (connector/mcp), behind an OAuth access token;
//   - /.well-known/oauth-*, /oauth/*: the authorization server for the one owner (connector/oauth).
//
// The web app's «Machine» shows the address to paste into claude.ai, the connections with Revoke and the journal of
// changes with Undo (connector/changes), from State.
//
// The tools are not here: a catalog package registers them on Registry (tools.Registry.Add, and Op for the undo
// steps no client sees) before the port serves, and Attach gives the tools the public port's app.
package connector

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/IvanIkra/infomaster/go/internal/clock"
	"github.com/IvanIkra/infomaster/go/internal/config"
	"github.com/IvanIkra/infomaster/go/internal/connector/changes"
	"github.com/IvanIkra/infomaster/go/internal/connector/mcp"
	"github.com/IvanIkra/infomaster/go/internal/connector/oauth"
	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/processing/news"
	"github.com/IvanIkra/infomaster/go/internal/store"
)

// Connector holds the connector's services; a tool finds it in tools.Context.Services.
type Connector struct {
	Env          config.Env
	Store        *store.Store
	News         *news.News
	Now          func() time.Time
	PublicOrigin string // scheme://host of PUBLIC_URL: an Origin /mcp accepts
	URL          string // PUBLIC_URL/mcp, what the owner pastes into claude.ai
	OAuth        *oauth.OAuth
	Changes      *changes.Changes
	Registry     *tools.Registry
	Endpoint     *mcp.Endpoint
	// App is the public port's app: whatever the wiring gives Attach, for the tools (tools.Context.App).
	App any
}

// New makes the connector's tables and services. sendCode is the bot's text to the owner (nil while there is no
// bot); lang, now and seconds may be nil (English, the clock).
func New(env config.Env, s *store.Store, newsList *news.News, sendCode func(context.Context, string) error,
	lang func() string, now func() time.Time, seconds func() float64) (*Connector, error) {
	if now == nil {
		now = clock.NowUTC
	}
	origin := env.PublicURL
	if scheme, rest, ok := strings.Cut(env.PublicURL, "://"); ok {
		host, _, _ := strings.Cut(rest, "/")
		host, _, _ = strings.Cut(host, "?")
		host, _, _ = strings.Cut(host, "#")
		origin = scheme + "://" + host
	}
	auth, err := oauth.New(s, env.PublicURL, sendCode, lang, seconds)
	if err != nil {
		return nil, err
	}
	journal, err := changes.New(s, now)
	if err != nil {
		return nil, err
	}
	c := &Connector{Env: env, Store: s, News: newsList, Now: now, PublicOrigin: origin, URL: env.PublicURL + "/mcp",
		OAuth: auth, Changes: journal, Registry: tools.NewRegistry()}
	c.Endpoint = &mcp.Endpoint{Registry: c.Registry, PublicOrigin: origin, Record: c.record,
		Context: func(r *http.Request, grantID *int64) *tools.Context { return c.Context(r.Context(), grantID) }}
	return c, nil
}

// Attach adds the connector's routes to the public port's mux and keeps app for the tools. Register the tools
// before the port serves.
func (c *Connector) Attach(mux *http.ServeMux, app any) {
	c.App = app
	c.OAuth.Routes(mux)
	mux.HandleFunc("/mcp", c.ServeMCP)
}

// Context is what a tool runs with: the app, the connector, the connection (nil for the owner).
func (c *Connector) Context(ctx context.Context, grantID *int64) *tools.Context {
	return &tools.Context{Ctx: ctx, App: c.App, Services: c, GrantID: grantID}
}

// ServeMCP is * /mcp: the token, then POST only.
func (c *Connector) ServeMCP(w http.ResponseWriter, r *http.Request) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	var grantID int64
	found := false
	if strings.ToLower(scheme) == "bearer" && token != "" {
		var err error
		if grantID, found, err = c.OAuth.Check(token); err != nil {
			httpx.WriteError(w, err)
			return
		}
	}
	if !found {
		challenge := `Bearer resource_metadata="` + c.OAuth.ResourceMetadata() + `"`
		body := "unauthorized"
		if token != "" {
			challenge += `, error="invalid_token", error_description="the token is unknown, revoked or expired"`
			body = "invalid_token"
		}
		httpx.JSONHeaders(w, http.StatusUnauthorized, httpx.Object{{Key: "error", Value: body}},
			map[string]string{"WWW-Authenticate": challenge})
		return
	}
	if r.Method != http.MethodPost { // no stream to open, no session to end
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	c.Endpoint.Post(w, r, grantID)
}

func (c *Connector) record(grantID *int64, tool string, arguments map[string]any, output tools.Output) (int64, error) {
	return c.Changes.Record(grantID, tool, arguments, output.Summary, output.Undo, output.Note, changes.ViaClaude)
}

// Act is a write tool called by the owner elsewhere (the web app, §8.6): checked like a client's call and journaled
// with its undo (via is changes.ViaWeb as a rule). A *tools.ToolError when the tool refuses.
func (c *Connector) Act(ctx context.Context, tool string, arguments map[string]any, via string) (int64,
	tools.Output, error) {
	found := c.Registry.Tool(tool)
	if found == nil || !found.Write {
		return 0, tools.Output{}, tools.Errorf("no such change: %s", tool)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	if problems := found.Check(arguments); len(problems) > 0 {
		return 0, tools.Output{}, &tools.ToolError{Msg: tools.Join(problems)}
	}
	output, err := found.Run(c.Context(ctx, nil), arguments)
	if err != nil {
		return 0, tools.Output{}, err
	}
	id, err := c.Changes.Record(nil, tool, arguments, output.Summary, output.Undo, output.Note, via)
	return id, output, err
}

// Journal is a change made without a tool (a question of the web app's «Check» put aside), for its undo.
func (c *Connector) Journal(tool string, arguments map[string]any, summary string, undo []tools.Step, note,
	via string) (int64, error) {
	return c.Changes.Record(nil, tool, arguments, summary, undo, note, via)
}

// Undo runs a change's undo steps; a *tools.ToolError with the reason when it cannot be done (a failed step's
// reason is kept in the journal).
func (c *Connector) Undo(ctx context.Context, changeID int64) (*changes.Change, error) {
	change, err := c.Changes.Get(changeID)
	if err != nil {
		return nil, err
	}
	if change == nil {
		return nil, tools.Errorf("no such change")
	}
	if change.UndoneAt != nil {
		return nil, tools.Errorf("it is undone already")
	}
	if change.Undo == nil {
		if change.Note != "" {
			return nil, &tools.ToolError{Msg: change.Note}
		}
		return nil, tools.Errorf("it cannot be undone")
	}
	toolContext := c.Context(ctx, nil)
	for _, step := range change.Undo {
		if err := c.Registry.Step(toolContext, step); err != nil {
			var refused *tools.ToolError
			if errors.As(err, &refused) {
				text := refused.Msg
				if err := c.Changes.Undone(changeID, &text); err != nil {
					return nil, err
				}
			}
			return nil, err
		}
	}
	if err := c.Changes.Undone(changeID, nil); err != nil {
		return nil, err
	}
	return c.Changes.Get(changeID)
}

// local is a unix time as local ISO; nil for 0.
func local(seconds int64) *string {
	if seconds == 0 {
		return nil
	}
	text := clock.LocalISO(time.Unix(seconds, 0))
	return &text
}

// State is «Machine → Claude connector»: the address, the connections, the journal of Claude's changes, how many
// tools read and write.
func (c *Connector) State() (httpx.Object, error) {
	connections, err := c.OAuth.Connections()
	if err != nil {
		return nil, err
	}
	listed := []httpx.Object{}
	for _, connection := range connections {
		listed = append(listed, httpx.Object{{Key: "id", Value: connection.ID}, {Key: "name", Value: connection.Name},
			{Key: "created_at", Value: local(connection.CreatedAt)}, {Key: "used_at", Value: local(connection.UsedAt)},
			{Key: "redirect", Value: connection.Redirect}})
	}
	recent, err := c.Changes.Recent(50, changes.ViaClaude)
	if err != nil {
		return nil, err
	}
	journal := []httpx.Object{}
	for _, change := range recent {
		journal = append(journal, httpx.Object{{Key: "id", Value: change.ID}, {Key: "at", Value: change.At},
			{Key: "tool", Value: change.Tool}, {Key: "summary", Value: change.Summary},
			{Key: "undo", Value: change.Undo != nil}, {Key: "note", Value: change.Note},
			{Key: "undone_at", Value: change.UndoneAt}, {Key: "undo_error", Value: change.UndoError}})
	}
	read, write := 0, 0
	for _, tool := range c.Registry.Tools() {
		if tool.Write {
			write++
		} else {
			read++
		}
	}
	return httpx.Object{{Key: "url", Value: c.URL}, {Key: "connections", Value: listed},
		{Key: "changes", Value: journal},
		{Key: "tools", Value: httpx.Object{{Key: "read", Value: read}, {Key: "write", Value: write}}}}, nil
}
