// Command compare asks two running services the same read-only questions and tells where their answers differ:
// the Python service and the Go one in the shadow mode on a copy of its database (18.5, D81).
//
//	compare -a http://infomaster:8081 -b http://infomaster-go:8081 [-public-a … -public-b …] [-calendar-secret …]
//
// The API token comes from -token or API_TOKEN and is never printed. JSON is compared as data (key order aside, 8
// and 8.0 equal); /api/status only by its keys and its errors, since the sources' times differ; iCal feeds byte for
// byte without their DTSTAMP lines; the web app's page by its status and its CSP. The exit status is 1 when anything
// differs, 2 on a usage error.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Check is one question: a path on one of the ports and how its answers are compared.
type Check struct {
	Port string // "internal" or "public"
	Path string
	Kind string // json, status, ical, page
	Auth bool   // with the Bearer token
}

// Checks is what is asked by default; the feeds come when their secrets are given.
func Checks(calendarSecret, publicSecret string) []Check {
	checks := []Check{
		{"internal", "/api/status", "status", true},
		{"internal", "/api/deadlines", "json", true},
		{"internal", "/api/news", "json", true},
		{"internal", "/api/grades", "json", true},
	}
	if calendarSecret != "" {
		for _, feed := range []string{"main", "optional", "tablo"} {
			checks = append(checks, Check{"internal", "/calendar/" + calendarSecret + "/" + feed + ".ics", "ical", false})
		}
	}
	checks = append(checks,
		Check{"public", "/.well-known/oauth-authorization-server", "json", false},
		Check{"public", "/.well-known/oauth-protected-resource", "json", false},
		Check{"public", "/web/", "page", false},
	)
	if publicSecret != "" {
		for _, feed := range []string{"main", "optional"} {
			checks = append(checks, Check{"public", "/calendar/" + publicSecret + "/" + feed + ".ics", "ical", false})
		}
	}
	return checks
}

// Answer is what one service said.
type Answer struct {
	Status int
	Header http.Header
	Body   []byte
}

// Compare tells how two answers to a check differ; nil when they agree.
func Compare(kind string, a, b Answer) []string {
	var diffs []string
	if a.Status != b.Status {
		diffs = append(diffs, fmt.Sprintf("status %d != %d", a.Status, b.Status))
	}
	if a.Status != http.StatusOK || b.Status != http.StatusOK {
		return diffs
	}
	switch kind {
	case "json":
		diffs = append(diffs, compareJSON(a.Body, b.Body)...)
	case "status":
		diffs = append(diffs, compareStatus(a.Body, b.Body)...)
	case "ical":
		if x, y := withoutStamps(a.Body), withoutStamps(b.Body); !bytes.Equal(x, y) {
			diffs = append(diffs, "the feed differs: "+firstLine(x, y))
		}
	case "page":
		if x, y := a.Header.Get("Content-Security-Policy"), b.Header.Get("Content-Security-Policy"); x != y {
			diffs = append(diffs, fmt.Sprintf("CSP %q != %q", x, y))
		}
		if x, y := a.Header.Get("Content-Type"), b.Header.Get("Content-Type"); x != y {
			diffs = append(diffs, fmt.Sprintf("Content-Type %q != %q", x, y))
		}
	}
	return diffs
}

func decode(body []byte) (any, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return normal(value), nil
}

// normal turns numbers into float64, so 8 and 8.0 are one value.
func normal(value any) any {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return v.String()
		}
		return f
	case map[string]any:
		for k, item := range v {
			v[k] = normal(item)
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = normal(item)
		}
		return v
	default:
		return value
	}
}

func compareJSON(a, b []byte) []string {
	x, err := decode(a)
	if err != nil {
		return []string{"a: not JSON: " + err.Error()}
	}
	y, err := decode(b)
	if err != nil {
		return []string{"b: not JSON: " + err.Error()}
	}
	var diffs []string
	walk("", x, y, &diffs)
	return diffs
}

// walk lists where two values differ, by their JSON path; at most 20 places.
func walk(at string, a, b any, diffs *[]string) {
	if len(*diffs) >= 20 {
		return
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s: an object != %s", path(at), short(b)))
			return
		}
		for _, key := range keys(x, y) {
			xv, xok := x[key]
			yv, yok := y[key]
			switch {
			case !yok:
				*diffs = append(*diffs, fmt.Sprintf("%s: only in a", path(at+"."+key)))
			case !xok:
				*diffs = append(*diffs, fmt.Sprintf("%s: only in b", path(at+"."+key)))
			default:
				walk(at+"."+key, xv, yv, diffs)
			}
		}
	case []any:
		y, ok := b.([]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s: a list != %s", path(at), short(b)))
			return
		}
		if len(x) != len(y) {
			*diffs = append(*diffs, fmt.Sprintf("%s: %d items != %d", path(at), len(x), len(y)))
		}
		for i := 0; i < len(x) && i < len(y); i++ {
			walk(fmt.Sprintf("%s[%d]", at, i), x[i], y[i], diffs)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*diffs = append(*diffs, fmt.Sprintf("%s: %s != %s", path(at), short(a), short(b)))
		}
	}
}

func keys(a, b map[string]any) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	list := make([]string, 0, len(seen))
	for k := range seen {
		list = append(list, k)
	}
	sort.Strings(list)
	return list
}

func path(at string) string {
	if at == "" {
		return "$"
	}
	return "$" + at
}

func short(value any) string {
	text, _ := json.Marshal(value)
	if len(text) > 80 {
		return string(text[:77]) + "..."
	}
	return string(text)
}

// compareStatus compares /api/status by the sources and their keys, and by the errors each source has: the times
// of the last polls differ between two services by nature.
func compareStatus(a, b []byte) []string {
	x, err := decode(a)
	if err != nil {
		return []string{"a: not JSON: " + err.Error()}
	}
	y, err := decode(b)
	if err != nil {
		return []string{"b: not JSON: " + err.Error()}
	}
	xs, ok1 := x.(map[string]any)
	ys, ok2 := y.(map[string]any)
	if !ok1 || !ok2 {
		return []string{"the status is not an object"}
	}
	var diffs []string
	for _, name := range keys(xs, ys) {
		xv, xok := xs[name].(map[string]any)
		yv, yok := ys[name].(map[string]any)
		if _, in := xs[name]; !in {
			diffs = append(diffs, name+": only in b")
			continue
		}
		if _, in := ys[name]; !in {
			diffs = append(diffs, name+": only in a")
			continue
		}
		if !xok || !yok {
			continue
		}
		for _, key := range keys(xv, yv) {
			_, xin := xv[key]
			_, yin := yv[key]
			switch {
			case !yin:
				diffs = append(diffs, name+"."+key+": only in a")
			case !xin:
				diffs = append(diffs, name+"."+key+": only in b")
			case strings.HasSuffix(key, "error") && (xv[key] == nil) != (yv[key] == nil):
				diffs = append(diffs, fmt.Sprintf("%s.%s: %s != %s", name, key, short(xv[key]), short(yv[key])))
			}
		}
	}
	return diffs
}

// withoutStamps drops the DTSTAMP lines (with their folded continuations): the time a feed was made.
func withoutStamps(feed []byte) []byte {
	var out bytes.Buffer
	skipping := false
	for _, line := range bytes.SplitAfter(feed, []byte("\n")) {
		if skipping && (bytes.HasPrefix(line, []byte(" ")) || bytes.HasPrefix(line, []byte("\t"))) {
			continue
		}
		skipping = bytes.HasPrefix(line, []byte("DTSTAMP"))
		if !skipping {
			out.Write(line)
		}
	}
	return out.Bytes()
}

// firstLine is the first line where two texts part.
func firstLine(a, b []byte) string {
	x, y := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := 0; i < len(x) || i < len(y); i++ {
		var l, r string
		if i < len(x) {
			l = x[i]
		}
		if i < len(y) {
			r = y[i]
		}
		if l != r {
			return fmt.Sprintf("line %d: %q != %q", i+1, strings.TrimRight(l, "\r"), strings.TrimRight(r, "\r"))
		}
	}
	return "only the line ends"
}

// Fetch asks one service.
func Fetch(client *http.Client, base string, check Check, token string) (Answer, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+check.Path, nil)
	if err != nil {
		return Answer{}, err
	}
	if check.Auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Answer{}, err
	}
	return Answer{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// Run asks both services every check and writes a line per check; it reports whether all agreed.
func Run(out io.Writer, client *http.Client, bases map[string][2]string, checks []Check, token string) bool {
	same := true
	for _, check := range checks {
		pair, ok := bases[check.Port]
		if !ok || pair[0] == "" || pair[1] == "" {
			fmt.Fprintf(out, "skip  %s (no %s addresses)\n", label(check), check.Port)
			continue
		}
		a, errA := Fetch(client, pair[0], check, token)
		b, errB := Fetch(client, pair[1], check, token)
		if errA != nil || errB != nil {
			same = false
			fmt.Fprintf(out, "FAIL  %s: a: %v, b: %v\n", label(check), errA, errB)
			continue
		}
		diffs := Compare(check.Kind, a, b)
		if len(diffs) == 0 {
			fmt.Fprintf(out, "same  %s\n", label(check))
			continue
		}
		same = false
		fmt.Fprintf(out, "DIFF  %s\n", label(check))
		for _, diff := range diffs {
			fmt.Fprintf(out, "      %s\n", diff)
		}
	}
	return same
}

// label is a check's path with the calendar secrets hidden.
func label(check Check) string {
	if strings.HasPrefix(check.Path, "/calendar/") {
		parts := strings.Split(check.Path, "/")
		return check.Port + " /calendar/<secret>/" + parts[len(parts)-1]
	}
	return check.Port + " " + check.Path
}

func main() {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	a := fs.String("a", "", "the internal port of service a (the running one), e.g. http://infomaster:8081")
	b := fs.String("b", "", "the internal port of service b (the shadow), e.g. http://infomaster-go:8081")
	publicA := fs.String("public-a", "", "the public port of service a, e.g. http://infomaster:8080")
	publicB := fs.String("public-b", "", "the public port of service b, e.g. http://infomaster-go:8080")
	token := fs.String("token", "", "the API token (default: API_TOKEN of the environment)")
	calendar := fs.String("calendar-secret", "", "CALENDAR_SECRET, to compare the internal feeds")
	public := fs.String("calendar-public-secret", "", "CALENDAR_PUBLIC_SECRET, to compare the public feeds")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *token == "" {
		*token = os.Getenv("API_TOKEN")
	}
	if *calendar == "" {
		*calendar = os.Getenv("CALENDAR_SECRET")
	}
	if *public == "" {
		*public = os.Getenv("CALENDAR_PUBLIC_SECRET")
	}
	if *a == "" || *b == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "compare: -a, -b and the token (-token or API_TOKEN) are required")
		os.Exit(2)
	}
	bases := map[string][2]string{"internal": {*a, *b}, "public": {*publicA, *publicB}}
	client := &http.Client{Timeout: 60 * time.Second}
	if !Run(os.Stdout, client, bases, Checks(*calendar, *public), *token) {
		os.Exit(1)
	}
}
