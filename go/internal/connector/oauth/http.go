package oauth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/i18n"
)

// FrameDeny are the headers of the authorize pages.
var FrameDeny = map[string]string{"X-Frame-Options": "DENY", "Content-Security-Policy": "frame-ancestors 'none'",
	"Referrer-Policy": "no-referrer", "Cache-Control": "no-store", "Pragma": "no-cache"}

// Routes adds the metadata and the /oauth/ endpoints to the public port's mux.
func (o *OAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", o.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", o.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", o.authorizationServer)
	mux.Handle("POST /oauth/register", httpx.Handle(o.register))
	mux.Handle("GET /oauth/authorize", httpx.Handle(o.authorize))
	mux.Handle("POST /oauth/authorize", httpx.Handle(o.authorizeForm))
	mux.Handle("POST /oauth/token", httpx.Handle(o.token))
	mux.Handle("POST /oauth/revoke", httpx.Handle(o.revoke))
}

func (o *OAuth) protectedResource(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, o.ProtectedResource())
}

func (o *OAuth) authorizationServer(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, o.AuthorizationServer())
}

func (e *Error) write(w http.ResponseWriter) {
	httpx.JSONHeaders(w, e.Status, e.Body(), NoStore)
}

func (o *OAuth) register(w http.ResponseWriter, r *http.Request) error {
	meta, err := httpx.ReadJSON(r, httpx.BodyMax)
	if errors.Is(err, httpx.ErrNotJSON) {
		oauthError("invalid_client_metadata", "expected JSON").write(w)
		return nil
	}
	if err != nil {
		return err
	}
	client, err := o.Register(meta)
	var refused *Error
	if errors.As(err, &refused) {
		refused.write(w)
		return nil
	}
	if err != nil {
		return err
	}
	httpx.JSONHeaders(w, http.StatusCreated, client, NoStore)
	return nil
}

// form is the body's fields (the last value of each) and the client_id of HTTP Basic when the body has none.
func form(r *http.Request) (Params, error) {
	values, err := httpx.ReadForm(r, httpx.BodyMax)
	if err != nil {
		return nil, err
	}
	fields := Last(values)
	scheme, credentials, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if _, given := fields["client_id"]; strings.ToLower(scheme) == "basic" && !given {
		// a client that sends its id the other way
		if decoded, ok := b64decode(credentials); ok {
			id, _, _ := strings.Cut(decoded, ":")
			fields["client_id"] = id
		}
	}
	return fields, nil
}

// b64decode is Python's base64.b64decode(...).decode(): characters outside the alphabet are dropped, the padding
// must be right and the bytes must be UTF-8.
func b64decode(text string) (string, bool) {
	var b strings.Builder
	for _, c := range text {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			b.WriteRune(c)
		}
	}
	data, err := base64.StdEncoding.DecodeString(b.String())
	if err != nil || !utf8Valid(data) {
		return "", false
	}
	return string(data), true
}

func (o *OAuth) token(w http.ResponseWriter, r *http.Request) error {
	fields, err := form(r)
	if err != nil {
		return err
	}
	tokens, err := o.Token(fields)
	var refused *Error
	if errors.As(err, &refused) {
		refused.write(w)
		return nil
	}
	if err != nil {
		return err
	}
	httpx.JSONHeaders(w, http.StatusOK, tokens, NoStore)
	return nil
}

func (o *OAuth) revoke(w http.ResponseWriter, r *http.Request) error {
	fields, err := form(r)
	if err != nil {
		return err
	}
	if token := fields["token"]; token != "" {
		if err := o.Revoke(token); err != nil {
			return err
		}
	}
	for name, value := range NoStore {
		w.Header().Set(name, value)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (o *OAuth) authorize(w http.ResponseWriter, r *http.Request) error {
	lang := o.Lang()
	pageID, page, err := o.OpenPage(First(r.URL.Query()))
	var refused *Error
	if errors.As(err, &refused) {
		if refused.Status == http.StatusFound {
			httpx.Redirect(w, http.StatusFound, refused.Location)
			return nil
		}
		o.page(w, lang, "", nil, refused.Description, http.StatusBadRequest)
		return nil
	}
	if err != nil {
		return err
	}
	o.page(w, lang, pageID, page, "", http.StatusOK)
	return nil
}

func (o *OAuth) authorizeForm(w http.ResponseWriter, r *http.Request) error {
	values, err := httpx.ReadForm(r, httpx.BodyMax)
	if err != nil {
		return err
	}
	fields := First(values)
	lang := o.Lang()
	pageID, action := fields["page"], fields.get("action")
	page := o.Page(pageID)
	if page == nil {
		o.page(w, lang, "", nil, i18n.Tr(lang, "This page has expired: connect again from Claude.",
			"Страница устарела: подключитесь заново из Claude."), http.StatusBadRequest)
		return nil
	}
	switch {
	case action != nil && *action == "deny":
		httpx.Redirect(w, http.StatusSeeOther, o.Deny(pageID))
	case action != nil && *action == "send":
		problem := o.Send(r.Context(), pageID)
		o.page(w, lang, pageID, page, problem, http.StatusOK)
	case action != nil && *action == "allow":
		location, err := o.Allow(pageID, fields.get("code"))
		var refused *Error
		if errors.As(err, &refused) {
			if still := o.Page(pageID); still != nil {
				o.page(w, lang, pageID, still, refused.Description, http.StatusBadRequest)
			} else {
				o.page(w, lang, "", page, refused.Description, http.StatusBadRequest)
			}
			return nil
		}
		if err != nil {
			return err
		}
		httpx.Redirect(w, http.StatusSeeOther, location)
	default:
		o.page(w, lang, pageID, page, "?", http.StatusBadRequest)
	}
	return nil
}

// escape is Python's html.escape: & < > " and '.
var escape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace

// PageTemplate is the authorize page; {lang}, {title} and {body} are filled in.
const PageTemplate = `<!doctype html>
<html lang="{lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>infomaster</title>
<style>
  :root { color-scheme: light dark; --fg: #111; --bg: #fff; --muted: #666; --accent: #2f6fed; --card: #f2f2f5; }
  @media (prefers-color-scheme: dark) { :root { --fg: #eee; --bg: #111; --muted: #aaa; --accent: #6d9cff;
    --card: #1e1e22; } }
  body { margin: 40px auto; max-width: 480px; padding: 0 16px; font: 17px/1.5 -apple-system, system-ui, sans-serif;
         color: var(--fg); background: var(--bg); }
  h1 { font-size: 22px; }
  .muted { color: var(--muted); font-size: 15px; }
  .error { color: #d33; }
  .card { background: var(--card); border-radius: 12px; padding: 12px 16px; }
  input { font: inherit; font-size: 24px; letter-spacing: 4px; width: 100%; box-sizing: border-box; padding: 10px;
          border-radius: 10px; border: 1px solid var(--muted); background: var(--bg); color: var(--fg); }
  button { font: inherit; padding: 10px 18px; border-radius: 10px; border: 0; margin: 12px 8px 0 0; cursor: pointer; }
  .primary { background: var(--accent); color: #fff; }
  .plain { background: var(--card); color: var(--fg); }
</style>
</head>
<body>
<h1>{title}</h1>
{body}
</body>
</html>
`

// page writes the authorize page: the request it answers (page may be nil), the problem, and the form when pageID
// is not "".
func (o *OAuth) page(w http.ResponseWriter, lang, pageID string, page *Page, problem string, status int) {
	var parts []string
	o.mu.Lock() // the page's code_hash changes under the lock
	codeSent := page != nil && page.CodeHash != nil
	o.mu.Unlock()
	if page != nil {
		host := urlsplit(page.RedirectURI).Hostname()
		where := host
		if contains(Loopback, host) {
			where = i18n.Tr(lang, "a program on this computer (a loopback address)",
				"программу на этом компьютере (loopback)")
		}
		parts = append(parts, `<p class="card"><b>`+escape(page.ClientName)+"</b> "+escape(i18n.Tr(lang,
			"wants to connect to infomaster. It will read and change your deadlines, timetable, grades, news, "+
				"chat messages and settings.",
			"хочет подключиться к infomaster. Он будет читать и менять дедлайны, расписание, оценки, новости, "+
				"сообщения чатов и настройки."))+"</p>")
		back := i18n.Tr(lang, "After you allow it, the browser goes back to ", "После разрешения браузер вернётся в ")
		parts = append(parts, `<p class="muted">`+escape(back)+"<b>"+escape(where)+"</b>.</p>")
	}
	if problem != "" {
		parts = append(parts, `<p class="error">`+escape(problem)+"</p>")
	}
	if pageID != "" && page != nil {
		hidden := `<input type="hidden" name="page" value="` + escape(pageID) + `">`
		if !codeSent {
			parts = append(parts, `<form method="post">`+hidden+"<p>"+escape(i18n.Tr(lang,
				"To prove it is you, the bot sends a one-time code to your private chat with it.",
				"Чтобы убедиться, что это вы, бот пришлёт одноразовый код в личный чат с ним."))+
				`</p><button class="primary" name="action" value="send">`+
				escape(i18n.Tr(lang, "Send a code to Telegram", "Прислать код в Telegram"))+
				`</button><button class="plain" name="action" value="deny">`+escape(i18n.Tr(lang, "Cancel", "Отмена"))+
				"</button></form>")
		} else {
			parts = append(parts, `<form method="post">`+hidden+"<p>"+escape(i18n.Tr(lang,
				"The bot sent a code to your private chat. Type it here:",
				"Бот прислал код в личный чат. Введите его:"))+
				`</p><input name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="12" autofocus>`+
				`<button class="primary" name="action" value="allow">`+escape(i18n.Tr(lang, "Allow", "Разрешить"))+
				`</button><button class="plain" name="action" value="send">`+
				escape(i18n.Tr(lang, "New code", "Новый код"))+`</button><button class="plain" name="action" `+
				`value="deny">`+escape(i18n.Tr(lang, "Deny", "Запретить"))+"</button></form>")
		}
	}
	title := i18n.Tr(lang, "Connect to infomaster", "Подключение к infomaster")
	text := strings.NewReplacer("{lang}", lang, "{title}", escape(title), "{body}", strings.Join(parts, "\n")).
		Replace(PageTemplate)
	for name, value := range FrameDeny {
		w.Header().Set(name, value)
	}
	httpx.HTML(w, status, text)
}

func utf8Valid(data []byte) bool { return utf8.Valid(data) }
