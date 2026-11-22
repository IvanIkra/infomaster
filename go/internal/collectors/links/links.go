// Package links reads PDFs behind links (§6.3, D59): a sheet often comes as a link, not as a file.
//
// Teachers post a task as a Dropbox link in a Smart LMS letter, a Google Drive or Yandex Disk link in a chat, or a
// plain URL of a PDF. A batch of a study chat or of the mailbox asks for the PDF behind the first such link of a
// message that has no file of its own (collectors/files), and the model then sees its first page as it sees a sent
// PDF.
//
// Only public links are followed, each by the address its service gives for a download: Dropbox by its content host
// (dl.dropboxusercontent.com: some resolvers block www.dropbox.com), a Drive file by `uc?export=download`, a Google
// document or presentation by its PDF export, a Yandex Disk file by the public API (a folder is not a sheet and is
// skipped). Pages behind a login (Google Classroom, Smart LMS itself) cannot be read and are not tried. What comes
// must start as a PDF does; anything else is no sheet.
package links

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	pubnet "github.com/IvanIkra/infomaster/go/internal/net"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

var (
	link = regexp.MustCompile(`https?://[^\p{Z}\t\n\v\f\r\x{1c}-\x{1f}\x{85}<>"'«»\[\]]+`)
	// The characters stripped from the end of a link: the sentence's, not the link's.
	trailing     = ".,;:!?)»"
	driveFile    = regexp.MustCompile(`^/file/d/([\p{L}\p{N}_-]{10,})`)
	googleExport = regexp.MustCompile(`^/(document|presentation|spreadsheets)/d/([\p{L}\p{N}_-]{10,})`)
	exports      = map[string]string{"document": "/document/d/%s/export?format=pdf",
		"presentation": "/presentation/d/%s/export/pdf", "spreadsheets": "/spreadsheets/d/%s/export?format=pdf"}
	loginHosts  = []string{"accounts.google.com", "classroom.google.com", "edu.hse.ru", "lms.hse.ru", "smartedu.hse.ru"}
	yandexHosts = []string{"disk.yandex.ru", "disk.yandex.com", "yadi.sk", "disk.360.yandex.ru"}

	filenameStar  = regexp.MustCompile(`filename\*=(?:UTF-8|utf-8)''([^;]+)`)
	filenamePlain = regexp.MustCompile(`filename="?([^";]+)"?`)
)

const (
	Timeout        = 120 * time.Second
	MaxLinks       = 3 // links of one message tried
	DropboxContent = "dl.dropboxusercontent.com"
)

// YandexAPI is the public resources API of Yandex Disk; a variable for tests.
var YandexAPI = "https://cloud-api.yandex.net/v1/disk/public/resources"

// LinkError is a link that could not be read now (the network, the service): it is tried again at a later run.
type LinkError struct{ Msg string }

func (e *LinkError) Error() string { return e.Msg }

func hasSuffix(host string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// DocumentLinks are the links of a text that may lead to a PDF, in order, each once: links of services that need a
// login are left out.
func DocumentLinks(text string) []string {
	var found []string
	for _, match := range link.FindAllString(text, -1) {
		url := strings.TrimRight(match, trailing)
		host := SplitURL(url).Host()
		if host != "" && !contains(found, url) && !hasSuffix(host, loginHosts) && Downloadable(url) {
			found = append(found, url)
		}
	}
	if len(found) > MaxLinks {
		found = found[:MaxLinks]
	}
	return found
}

// Downloadable reports a link of a service this package reads, or a URL of a PDF.
func Downloadable(url string) bool {
	parts := SplitURL(url)
	host := parts.Host()
	if strings.HasSuffix(host, "dropbox.com") && (strings.HasPrefix(parts.Path, "/s/") ||
		strings.HasPrefix(parts.Path, "/scl/fi/")) {
		return true
	}
	if host == "drive.google.com" && (driveFile.MatchString(parts.Path) ||
		parts.Path == "/open" && strings.Contains(parts.Query, "id=")) {
		return true
	}
	if host == "docs.google.com" && googleExport.MatchString(parts.Path) {
		return true
	}
	if contains(yandexHosts, host) {
		return true
	}
	return strings.HasSuffix(strings.ToLower(parts.Path), ".pdf")
}

// Direct is the address a PDF is downloaded from. A Yandex Disk link stays as it is: it needs the API (Fetch).
func Direct(url string) string {
	parts := SplitURL(url)
	host := parts.Host()
	if strings.HasSuffix(host, "dropbox.com") { // its content host: www.dropbox.com is blocked by some resolvers
		var query []Pair
		for _, p := range ParseQSL(parts.Query, true) {
			if p.Key != "dl" {
				query = append(query, p)
			}
		}
		query = append(query, Pair{"dl", "1"})
		return URL{Scheme: "https", Netloc: DropboxContent, Path: parts.Path, Query: URLEncode(query)}.String()
	}
	if host == "drive.google.com" {
		var fileID string
		if found := driveFile.FindStringSubmatch(parts.Path); found != nil {
			fileID = found[1]
		} else {
			for _, p := range ParseQSL(parts.Query, false) {
				if p.Key == "id" {
					fileID = p.Value // the last one, as dict() keeps it
				}
			}
		}
		return "https://drive.google.com/uc?export=download&id=" + pytext.Quote(fileID, "/")
	}
	if host == "docs.google.com" {
		if found := googleExport.FindStringSubmatch(parts.Path); found != nil {
			return "https://docs.google.com" + fmt.Sprintf(exports[found[1]], found[2])
		}
	}
	return url
}

// FileName is the file's name: Content-Disposition, else the last part of the URL's path; always ending in .pdf.
func FileName(disposition, url string) string {
	var name string
	if found := filenameStar.FindStringSubmatch(disposition); found != nil {
		name = pytext.Strip(Unquote(found[1]))
	} else if found := filenamePlain.FindStringSubmatch(disposition); found != nil {
		name = pytext.Strip(Unquote(found[1]))
	} else {
		path := SplitURL(url).Path
		name = pytext.Strip(Unquote(path[strings.LastIndex(path, "/")+1:]))
	}
	if name == "" {
		name = "document.pdf"
	}
	if strings.HasSuffix(strings.ToLower(name), ".pdf") {
		return name
	}
	return name + ".pdf"
}

func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "infomaster")
	return client.Do(req)
}

// save downloads url into path; its name, or "" when it is no PDF or too big.
func save(ctx context.Context, client *http.Client, url, path string, limit int64) (string, error) {
	resp, err := get(ctx, client, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 410:
		return "", nil
	case resp.StatusCode >= 400:
		return "", &LinkError{fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if resp.ContentLength > limit {
		return "", nil
	}
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer out.Close()
	head := make([]byte, 5)
	n, err := io.ReadFull(resp.Body, head)
	if n == 0 && (err == io.EOF || err == nil) {
		return "", nil
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", err
	}
	if !bytes.Equal(head[:n], []byte("%PDF-")) {
		return "", nil // a page, a login form, a Word file: no sheet
	}
	if _, err := out.Write(head); err != nil {
		return "", err
	}
	copied, err := io.Copy(out, io.LimitReader(resp.Body, limit-int64(n)+1))
	if err != nil {
		return "", err
	}
	if int64(n)+copied > limit {
		return "", nil
	}
	return FileName(resp.Header.Get("Content-Disposition"), resp.Request.URL.String()), nil
}

// yandex is the download address and the name of a public Yandex Disk file; ok false for a folder or a file that
// is no PDF.
func yandex(ctx context.Context, client *http.Client, url string) (address, name string, ok bool, err error) {
	query := URLEncode([]Pair{{"public_key", url}, {"fields", "type,name,mime_type,file"}})
	resp, err := get(ctx, client, YandexAPI+"?"+query)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return "", "", false, nil
	}
	if resp.StatusCode >= 400 {
		return "", "", false, &LinkError{fmt.Sprintf("Yandex Disk: HTTP %d", resp.StatusCode)}
	}
	var about struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		MimeType string `json:"mime_type"`
		File     string `json:"file"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&about); err != nil {
		return "", "", false, &LinkError{"Yandex Disk: " + err.Error()}
	}
	if about.Type != "file" || about.File == "" {
		return "", "", false, nil
	}
	name = about.Name
	if name == "" {
		name = "document.pdf"
	}
	if about.MimeType != "application/pdf" && !strings.HasSuffix(strings.ToLower(name), ".pdf") {
		return "", "", false, nil
	}
	return about.File, name, true, nil
}

// NewClient makes the client Fetch uses: public addresses only (D79). A variable for tests.
var NewClient = func() *http.Client { return pubnet.Client(Timeout) }

// Fetch is Files.fetch_url: the PDF behind a link into path; its name, or "" when there is none (a folder, a page,
// a file that is no PDF, a link that needs a login, a link into the service's own network). *LinkError when the
// service or the network fails.
func Fetch(ctx context.Context, url, path string, limit int64) (string, error) {
	client := NewClient()
	name, err := fetch(ctx, client, url, path, limit)
	var private *pubnet.PrivateAddress
	var linkErr *LinkError
	var pathErr *os.PathError
	switch {
	case err == nil:
		return name, nil
	case errors.As(err, &private):
		return "", nil // a link into the service's own network is no sheet (D79)
	case errors.As(err, &linkErr), errors.As(err, &pathErr):
		return "", err
	default:
		msg := err.Error()
		if msg == "" {
			msg = fmt.Sprintf("%T", err)
		}
		return "", &LinkError{msg}
	}
}

func fetch(ctx context.Context, client *http.Client, url, path string, limit int64) (string, error) {
	if contains(yandexHosts, SplitURL(url).Host()) {
		address, name, ok, err := yandex(ctx, client, url)
		if err != nil || !ok {
			return "", err
		}
		saved, err := save(ctx, client, address, path, limit)
		if err != nil || saved == "" {
			return "", err
		}
		return name, nil
	}
	return save(ctx, client, Direct(url), path, limit)
}
