package web

// The web app (§8.6, D61): its files, the login by a code from the bot, sessions per device (tests/test_web.py).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFiles(t *testing.T) {
	h := newHarness(t, options{lang: "ru"}).serve()
	page := h.get("/web/")
	if page.Code != 200 || page.Header().Get("Cache-Control") != "no-cache" ||
		!strings.Contains(page.Body.String(), `<script type="importmap">`) {
		t.Fatalf("index: %d %v", page.Code, page.Header())
	}
	if page.Header().Get("Content-Type") != "text/html" {
		t.Fatal(page.Header().Get("Content-Type"))
	}
	if r := h.get("/web"); r.Code != 302 || r.Header().Get("Location") != "/web/" {
		t.Fatal(r.Code, r.Header())
	}
	if r := h.get("/web/sw.js"); r.Header().Get("Content-Type") != "text/javascript" || r.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal(r.Header())
	}
	manifest := h.get("/web/manifest.webmanifest")
	if manifest.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatal(manifest.Header())
	}
	var parsed map[string]any
	noErr(t, json.Unmarshal(manifest.Body.Bytes(), &parsed))
	if parsed["scope"] != "/web/" {
		t.Fatal(parsed["scope"])
	}
	for _, path := range []string{"app.js", "app.css", "fonts.css", "vendor/preact.module.js", "icons/icon-512.png",
		"fonts/Lora-normal-cyrillic.woff2"} {
		r := h.get("/web/static/" + path)
		if r.Code != 200 || r.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal(path, r.Code, r.Header())
		}
	}
	types := map[string]string{"app.js": "text/javascript", "app.css": "text/css", "icons/icon-512.png": "image/png",
		"fonts/Lora-normal-cyrillic.woff2": "font/woff2"}
	for path, want := range types {
		if got := h.get("/web/static/" + path).Header().Get("Content-Type"); got != want {
			t.Fatal(path, got)
		}
	}
	// a 304 while unchanged, a range for iOS media, nothing outside the directory
	first := h.get("/web/static/app.js")
	request := httptest.NewRequest("GET", "/web/static/app.js", nil)
	request.Header.Set("If-None-Match", first.Header().Get("ETag"))
	again := httptest.NewRecorder()
	h.handler.ServeHTTP(again, request)
	if again.Code != http.StatusNotModified {
		t.Fatal(again.Code)
	}
	request = httptest.NewRequest("GET", "/web/static/app.js", nil)
	request.Header.Set("Range", "bytes=0-9")
	part := httptest.NewRecorder()
	h.handler.ServeHTTP(part, request)
	if part.Code != http.StatusPartialContent || part.Body.Len() != 10 {
		t.Fatal(part.Code, part.Body.Len())
	}
	for _, path := range []string{"/web/static/../web.py", "/web/static/%2e%2e/__init__.py", "/web/static/nothing.js",
		"/web/static/fonts/"} {
		if r := h.get(path); r.Code == 200 {
			t.Fatal(path, r.Code)
		}
	}
	if r := h.get("/web/nothing"); r.Code != 404 || r.Body.String() != "404: Not Found" ||
		r.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal(r.Code, r.Body, r.Header())
	}
	share := httptest.NewRecorder()
	h.handler.ServeHTTP(share, httptest.NewRequest("POST", "/web/share", strings.NewReader("text=x")))
	if share.Code != 303 || share.Header().Get("Location") != "/web/#/add?shared=lost" {
		t.Fatal(share.Code, share.Header())
	}
}

func TestThePagesRunOnlyTheirOwnScripts(t *testing.T) {
	h := newHarness(t, options{}).serve()
	page := h.get("/web/")
	csp := page.Header().Get("Content-Security-Policy")
	found := regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`).FindStringSubmatch(page.Body.String())
	sum := sha256.Sum256([]byte(found[1]))
	scriptSrc := strings.Split(strings.Split(csp, "script-src")[1], ";")[0]
	if scriptSrc != " 'self' 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'" {
		t.Fatal(scriptSrc)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") || page.Header().Get("X-Frame-Options") != "DENY" ||
		page.Header().Get("X-Content-Type-Options") != "nosniff" || page.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatal(page.Header())
	}
	api := h.get("/web/api/me")
	if api.Header().Get("Cache-Control") != "no-store" || api.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(api.Header())
	}
	// the CSP as Python computed it from the same index.html
	var vectors struct {
		CSP   string `json:"csp"`
		Index string `json:"index_sha256"`
	}
	noErr(t, json.Unmarshal(must(os.ReadFile("testdata/python_vectors.json")), &vectors))
	index := sha256.Sum256(must(os.ReadFile(filepath.Join(FindStatic(), "index.html"))))
	if fmt.Sprintf("%x", index) != vectors.Index {
		t.Skip("index.html changed since the vectors were made: the check above still holds")
	}
	if csp != vectors.CSP {
		t.Fatalf("CSP differs from Python's:\n%s\n%s", csp, vectors.CSP)
	}
}

func TestEverythingStaticIsReferencedFilesExist(t *testing.T) {
	static := FindStatic()
	index := string(must(os.ReadFile(filepath.Join(static, "index.html"))))
	names := []string{}
	for _, m := range regexp.MustCompile(`"/web/static/([^"]+)"`).FindAllStringSubmatch(index, -1) {
		names = append(names, m[1])
	}
	fonts := string(must(os.ReadFile(filepath.Join(static, "fonts.css"))))
	for _, m := range regexp.MustCompile(`url\(fonts/([^)]+)\)`).FindAllStringSubmatch(fonts, -1) {
		names = append(names, "fonts/"+m[1])
	}
	var manifest struct {
		Icons []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	noErr(t, json.Unmarshal(must(os.ReadFile(filepath.Join(static, "manifest.webmanifest"))), &manifest))
	for _, icon := range manifest.Icons {
		names = append(names, strings.TrimPrefix(icon.Src, "/web/static/"))
	}
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(static, name)); err != nil || info.IsDir() {
			t.Fatal(name)
		}
	}
}

// --- the code from the bot ---------------------------------------------------------------------------------------

func TestCodeLogin(t *testing.T) {
	h := newHarness(t, options{lang: "ru"}).serve()
	if r := h.call("GET", "/me", nil, req{}); r.Code != 401 || obj(decode(t, r))["error"] != "log in" {
		t.Fatal(r.Code, r.Body)
	}
	if r := h.call("POST", "/login/code", map[string]any{}, req{}); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if !strings.Contains(h.texts()[0], "код для входа в приложение infomaster") {
		t.Fatal(h.texts())
	}
	code := h.code()
	wrong := "000000"
	if code == wrong {
		wrong = "1"
	}
	r := h.call("POST", "/login/code/check", map[string]any{"code": wrong}, req{})
	if r.Code != 400 || obj(decode(t, r))["error"] != "Неверный код." {
		t.Fatal(r.Code, r.Body)
	}
	spaced := strings.Join(strings.Split(code, ""), " ")
	r = h.call("POST", "/login/code/check", map[string]any{"code": spaced}, req{})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	cookie := r.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/web" ||
		cookie.MaxAge != SessionIdle {
		t.Fatal(cookie)
	}
	me := h.call("GET", "/me", nil, req{token: cookie.Value})
	if me.Body.String() != `{"lang": "ru", "session": 1, "device": "iPhone \u00b7 Safari", "passkeys": 0, "push": false}` {
		t.Fatal(me.Body.String())
	}
	again := h.call("POST", "/login/code/check", map[string]any{"code": code}, req{})
	if again.Code != 400 || obj(decode(t, again))["error"] != "Сначала запросите код." {
		t.Fatal(again.Body)
	}
}

func TestCodeLimits(t *testing.T) {
	h := newHarness(t, options{lang: "ru"}).serve()
	code := func() int { return h.call("POST", "/login/code", map[string]any{}, req{}).Code }
	if code() != 200 || code() != 429 {
		t.Fatal("a moment ago")
	}
	for range CodesPerHour - 1 {
		h.secs.add(ResendAfter)
		if code() != 200 {
			t.Fatal("hour")
		}
	}
	h.secs.add(ResendAfter)
	r := h.call("POST", "/login/code", map[string]any{}, req{})
	if r.Code != 429 || !strings.Contains(obj(decode(t, r))["error"].(string), "Слишком много кодов") {
		t.Fatal(r.Body)
	}
	h.secs.add(3600)
	if code() != 200 {
		t.Fatal("next hour")
	}
	for range CodesPerDay - CodesPerHour - 1 {
		h.secs.add(600)
		if code() != 200 {
			t.Fatal("day")
		}
	}
	h.secs.add(3600)
	if code() != 429 {
		t.Fatal("the day's limit")
	}
	h.secs.add(86400)
	if code() != 200 {
		t.Fatal("next day")
	}
}

func TestCodesAskedAtOncePassTheLimitsOnce(t *testing.T) {
	h := newHarness(t, options{}).serve()
	var wg sync.WaitGroup
	statuses := make([]int, 20)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = h.call("POST", "/login/code", map[string]any{}, req{}).Code
		}(i)
	}
	wg.Wait()
	sort.Ints(statuses)
	if statuses[0] != 200 || statuses[1] != 429 || statuses[19] != 429 || len(h.texts()) != 1 {
		t.Fatal(statuses, len(h.texts()))
	}
}

func TestCodeAttemptsAndAge(t *testing.T) {
	h := newHarness(t, options{}).serve()
	h.call("POST", "/login/code", map[string]any{}, req{})
	right := h.code()
	for range Attempts {
		h.call("POST", "/login/code/check", map[string]any{"code": "x"}, req{})
	}
	if r := h.call("POST", "/login/code/check", map[string]any{"code": right}, req{}); r.Code != 400 {
		t.Fatal("dropped")
	}
	h.secs.add(ResendAfter)
	h.call("POST", "/login/code", map[string]any{}, req{})
	h.secs.add(CodeTTL + 1)
	if r := h.call("POST", "/login/code/check", map[string]any{"code": h.code()}, req{}); r.Code != 400 {
		t.Fatal("old")
	}
	if r := h.call("POST", "/login/code/check", nil, req{}); r.Code != 400 || obj(decode(t, r))["error"] != "JSON expected" {
		t.Fatal(r.Body)
	}
}

func TestBotDown(t *testing.T) {
	failing := newHarness(t, options{sendCode: func(context.Context, string) error { return fmt.Errorf("blocked") }}).serve()
	if r := failing.call("POST", "/login/code", map[string]any{}, req{}); r.Code != 502 {
		t.Fatal(r.Code)
	}
	none := newHarness(t, options{noBot: true}).serve()
	if r := none.call("POST", "/login/code", map[string]any{}, req{}); r.Code != 503 {
		t.Fatal(r.Code)
	}
}

func TestOrigin(t *testing.T) {
	h := newHarness(t, options{}).serve()
	if r := h.call("POST", "/login/code", map[string]any{}, req{origin: noOrigin()}); r.Code != 403 ||
		r.Body.String() != `{"error": "wrong origin"}` {
		t.Fatal(r.Code, r.Body)
	}
	if r := h.call("POST", "/login/code", map[string]any{}, req{origin: ptr("https://evil.example")}); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if len(h.texts()) != 0 {
		t.Fatal(h.texts())
	}
	token := h.logIn(iphone)
	if r := h.call("POST", "/logout", map[string]any{}, req{token: token, origin: ptr("https://evil.example")}); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if r := h.call("GET", "/me", nil, req{token: token, origin: noOrigin()}); r.Code != 200 {
		t.Fatal("reading needs no origin")
	}
}

// --- sessions ----------------------------------------------------------------------------------------------------

func TestSessions(t *testing.T) {
	h := newHarness(t, options{}).serve()
	phone := h.logIn(iphone)
	desktop := h.logIn(mac)
	listed := obj(decode(t, h.call("GET", "/devices", nil, req{token: phone})))
	devices := []string{}
	for _, s := range list(listed["sessions"]) {
		devices = append(devices, obj(s)["device"].(string))
	}
	if listed["current"] != int64(1) || strings.Join(devices, "|") != "Mac · Chrome|iPhone · Safari" {
		t.Fatal(listed)
	}
	after := obj(decode(t, h.call("DELETE", "/sessions/2", nil, req{token: phone})))
	if len(list(after["sessions"])) != 1 || obj(list(after["sessions"])[0])["id"] != int64(1) {
		t.Fatal(after)
	}
	if h.call("GET", "/me", nil, req{token: desktop}).Code != 401 {
		t.Fatal("ended")
	}
	if r := h.call("DELETE", "/sessions/2", nil, req{token: phone}); r.Code != 404 || obj(decode(t, r))["error"] != "no such device" {
		t.Fatal(r.Body)
	}
	if r := h.call("DELETE", "/sessions/x", nil, req{token: phone}); r.Code != 404 || obj(decode(t, r))["error"] != "Not Found" {
		t.Fatal(r.Body)
	}
	out := h.call("POST", "/logout", map[string]any{}, req{token: phone})
	if out.Code != 200 || !strings.Contains(out.Header().Get("Set-Cookie"), "im_session=;") ||
		!strings.Contains(out.Header().Get("Set-Cookie"), "Path=/web") {
		t.Fatal(out.Header())
	}
	if h.call("GET", "/me", nil, req{token: phone}).Code != 401 {
		t.Fatal("logged out")
	}
}

func TestIdleSession(t *testing.T) {
	h := newHarness(t, options{}).serve()
	token := h.logIn(iphone)
	h.secs.add(SessionIdle - 10)
	if h.call("GET", "/me", nil, req{token: token}).Code != 200 {
		t.Fatal("its use is noted")
	}
	h.secs.add(SessionIdle - 10)
	if h.call("GET", "/me", nil, req{token: token}).Code != 200 {
		t.Fatal("again")
	}
	h.secs.add(SessionIdle + 1)
	if h.call("GET", "/me", nil, req{token: token}).Code != 401 {
		t.Fatal("idle")
	}
}

func TestASessionHasAnEndHoweverUsed(t *testing.T) {
	h := newHarness(t, options{}).serve()
	token := h.logIn(iphone)
	for range SessionMax / SessionIdle {
		h.secs.add(SessionIdle - 10)
		if h.call("GET", "/me", nil, req{token: token}).Code != 200 {
			t.Fatal("in use")
		}
	}
	h.secs.add(SessionMax%SessionIdle + 100)
	if h.call("GET", "/me", nil, req{token: token}).Code != 401 {
		t.Fatal("too old")
	}
}

func TestDeviceName(t *testing.T) {
	cases := map[string]string{iphone: "iPhone · Safari", mac: "Mac · Chrome",
		"Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Mobile Safari/537.36": "Android · Chrome",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0":                                  "Windows · Firefox",
		"": "?"}
	for agent, want := range cases {
		if got := DeviceName(agent); got != want {
			t.Fatal(agent, got)
		}
	}
}

func TestPasskeysWithoutAVerifierAreRefused(t *testing.T) {
	h := newHarness(t, options{lang: "ru"}).serve().in()
	options := obj(decode(t, h.call("POST", "/passkeys/options", map[string]any{}, req{token: h.token})))
	if obj(options["rp"])["id"] != testRPID || obj(options["authenticatorSelection"])["userVerification"] != "required" ||
		len(list(options["excludeCredentials"])) != 0 || options["timeout"] != int64(60000) ||
		obj(options["user"])["id"] != "aW5mb21hc3Rlci1vd25lcg" || len(options["challenge"].(string)) != 86 {
		t.Fatal(options)
	}
	login := obj(decode(t, h.call("POST", "/login/passkey/options", map[string]any{}, req{})))
	if login["rpId"] != testRPID || login["userVerification"] != "required" || len(list(login["allowCredentials"])) != 0 {
		t.Fatal(login)
	}
	if r := h.call("POST", "/login/passkey", map[string]any{"credential": "nonsense"}, req{}); r.Code != 400 {
		t.Fatal(r.Code)
	}
	if r := h.call("POST", "/passkeys", map[string]any{"credential": map[string]any{}}, req{}); r.Code != 401 {
		t.Fatal(r.Code)
	}
}

// --- the screens (stage 16.2) ------------------------------------------------------------------------------------

func screens(t *testing.T) *harness {
	h := newHarness(t, options{screens: true, data: func(h *harness, d *Data) {
		must(h.sch.Subjects.SetManual("Discrete Mathematics", "Discrete Mathematics", "DM"))
		must(h.sch.Subjects.SetManual("Algebra", "Algebra", "Alg"))
		must(h.sch.Inbox.PutChat(testChat, "DM seminar chat", "supergroup", nil, 0))
	}})
	return h.serve().in()
}

func TestScreensNeedASession(t *testing.T) {
	h := screens(t)
	for _, path := range []string{"/feed", "/week", "/deadline?uid=x", "/class?uid=x", "/course?key=DM"} {
		if h.call("GET", path, nil, req{}).Code != 401 {
			t.Fatal(path)
		}
	}
}

func items(feed map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, day := range list(feed["days"]) {
		for _, item := range list(obj(day)["items"]) {
			out[obj(day)["date"].(string)] = append(out[obj(day)["date"].(string)], obj(item))
		}
	}
	return out
}

func TestFeed(t *testing.T) {
	h := screens(t)
	h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics") // due at Thursday's seminar
	h.addRow("Essay", "2026-10-25", "", "")
	h.addRow("Old quiz", "2026-10-19", "", "Algebra")
	feed := h.read("/feed")
	if feed["today"] != "2026-10-20" || feed["now"] != "2026-10-20T12:00" {
		t.Fatal(feed["today"], feed["now"])
	}
	current := obj(feed["current"])
	if current["title"] != "Algebra" || current["start"] != "2026-10-20T13:00:00" {
		t.Fatal(current)
	}
	if overdue := list(feed["overdue"]); len(overdue) != 1 || obj(overdue[0])["title"] != "Old quiz" {
		t.Fatal(overdue)
	}
	if first := obj(list(feed["courses"])[0]); first["key"] != "Alg" || first["title"] != "Algebra" {
		t.Fatal(first)
	}
	days := items(feed)
	thursday := days["2026-10-22"]
	if len(thursday) != 2 || thursday[0]["title"] != "Discrete Mathematics" || thursday[1]["title"] != "Sheet 5" {
		t.Fatal(thursday)
	}
	if len(days["2026-10-25"]) != 1 || days["2026-10-25"][0]["title"] != "Essay" {
		t.Fatal(days["2026-10-25"])
	}
	if obj(thursday[1]["at_class"])["uid"] != thursday[0]["uid"] || thursday[1]["course"] != "DM" {
		t.Fatal(thursday[1])
	}
	due := list(thursday[0]["due"])
	if len(due) != 1 || obj(due[0])["uid"] != thursday[1]["uid"] || obj(due[0])["title"] != "Sheet 5" {
		t.Fatal(due)
	}
	if thursday[0]["online"] != true || thursday[1]["from"] != "DM seminar chat" {
		t.Fatal(thursday)
	}
	lens := h.read("/feed?course=DM")
	titles := []string{}
	for _, day := range list(lens["days"]) {
		for _, item := range list(obj(day)["items"]) {
			titles = append(titles, obj(item)["title"].(string))
		}
	}
	if strings.Join(titles, "|") != "Discrete Mathematics|Sheet 5" || len(list(lens["overdue"])) != 0 ||
		lens["current"] != nil {
		t.Fatal(titles, lens["current"])
	}
	if _, ok := feed["check"]; !ok || feed["problems"] != int64(0) || feed["inbox"] != int64(0) {
		t.Fatal(feed["check"], feed["problems"], feed["inbox"])
	}
}

func TestWeek(t *testing.T) {
	h := screens(t)
	h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics")
	week := h.read("/week?start=2026-10-21")
	if week["start"] != "2026-10-19" || len(list(week["days"])) != 7 {
		t.Fatal(week["start"])
	}
	monday := list(obj(list(week["days"])[0])["items"])
	if obj(monday[0])["title"] != "Математический анализ" || obj(monday[0])["past"] != true {
		t.Fatal(monday[0]) // no dictionary entry yet
	}
	kinds := []string{}
	for _, item := range list(obj(list(week["days"])[3])["items"]) {
		kinds = append(kinds, obj(item)["kind"].(string))
	}
	if strings.Join(kinds, ",") != "class,deadline" {
		t.Fatal(kinds)
	}
	if r := h.call("GET", "/week?start=soon", nil, req{token: h.token}); r.Code != 400 {
		t.Fatal(r.Code)
	}
}

func TestDeadlineCard(t *testing.T) {
	h := screens(t)
	at := h.clock.now()
	must(h.sch.Inbox.Add(testChat, 1, inboxFields("Starosta", "Sheet 5 is due on Sunday", at), "tg", time.Time{}))
	first := h.addRow("Sheet 5", "2026-10-25", "", "Discrete Mathematics")
	moved := h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics")
	must(h.sch.Deadlines.Move(first, moved))
	feed := h.read("/feed")
	var uid string
	for _, list := range items(feed) {
		for _, item := range list {
			if item["kind"] == "deadline" {
				uid = item["uid"].(string)
			}
		}
	}
	card := h.read("/deadline?uid=" + uid)
	if card["title"] != "Sheet 5" || obj(card["at_class"])["title"] != "Discrete Mathematics" {
		t.Fatal(card)
	}
	quote := obj(card["quote"])
	if quote["text"] != "Sheet 5 is due on Sunday" || quote["sender"] != "Starosta" {
		t.Fatal(quote)
	}
	history := list(card["history"])
	if len(history) != 2 || obj(history[0])["what"] != "found" || obj(history[0])["due"] != "2026-10-25" ||
		obj(history[1])["what"] != "moved" || obj(history[1])["was"] != "2026-10-25" || obj(history[1])["due"] != "2026-10-22" {
		t.Fatal(history)
	}
	if r := h.call("GET", "/deadline?uid=nothing", nil, req{token: h.token}); r.Code != 404 {
		t.Fatal(r.Code)
	}
}

func TestClassCard(t *testing.T) {
	h := screens(t)
	h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics")
	var uid string
	for _, list := range items(h.read("/feed")) {
		for _, item := range list {
			if item["title"] == "Discrete Mathematics" {
				uid = item["uid"].(string)
			}
		}
	}
	card := h.read("/class?uid=" + uid)
	due := list(card["due"])
	if card["type"] != "Seminar" || card["course"] != "DM" || card["course_title"] != "Discrete Mathematics" ||
		len(due) != 1 || obj(due[0])["title"] != "Sheet 5" || card["materials"] != int64(0) || card["quote"] != nil {
		t.Fatal(card)
	}
	if r := h.call("GET", "/class?uid=nothing", nil, req{token: h.token}); r.Code != 404 {
		t.Fatal(r.Code)
	}
}

func TestCourse(t *testing.T) {
	h := screens(t)
	h.addRow("Sheet 5", "2026-10-22", "", "Discrete Mathematics")
	lens := h.read("/course?key=DM")
	next := obj(lens["next"])
	if lens["title"] != "Discrete Mathematics" || next["start"] != "2026-10-22T16:40:00" ||
		obj(list(next["due"])[0])["title"] != "Sheet 5" {
		t.Fatal(lens)
	}
	r := h.call("GET", "/course?key=DM", nil, req{token: h.token})
	body := r.Body.String()
	if !strings.Contains(body, `"grades": {"count": 0, "pending": 0, "new": 0, "average": null, "works": [], "books": []}`) ||
		!strings.Contains(body, `"documents": []`) || !strings.Contains(body, `"sheets": {"count": 0}`) {
		t.Fatal(body)
	}
	if r := h.call("GET", "/course?key=Nope", nil, req{token: h.token}); r.Code != 404 {
		t.Fatal(r.Code)
	}
}
