package web

// The handlers of «+» (stage 16.5): the class material goes to, one line read by the model, files and notes for the
// library, a deadline, an event or a grade by hand. The part of web/__init__.py that serves quick.go.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/IvanIkra/infomaster/go/internal/connector/tools"
	"github.com/IvanIkra/infomaster/go/internal/httpx"
	"github.com/IvanIkra/infomaster/go/internal/library"
	libfeed "github.com/IvanIkra/infomaster/go/internal/library/feed"
	"github.com/IvanIkra/infomaster/go/internal/pytext"
)

var quickErrors = map[string]struct {
	status int
	en, ru string
}{
	"unavailable": {503, "One line needs the Anthropic key: add it by hand.",
		"Для одной строки нужен ключ Anthropic: добавьте вручную."},
	"budget": {429, "The daily LLM limit is spent: add it by hand.", "Дневной лимит LLM исчерпан: добавьте вручную."},
	"empty":  {400, "Type what to add.", "Напишите, что добавить."},
}

func (c *call) quick() (*Quick, error) {
	if c.app.Quick == nil {
		return nil, httpx.Fail(http.StatusServiceUnavailable, "the screens are not wired")
	}
	return c.app.Quick, nil
}

// addState is what «+» shows: the class in progress and the recent ones, the courses, whether one line can be read.
func addState(c *call) error {
	data, err := c.data()
	if err != nil {
		return err
	}
	quick, err := c.quick()
	if err != nil {
		return err
	}
	place, err := quick.Place()
	if err != nil {
		return err
	}
	courses, err := data.Courses()
	if err != nil {
		return err
	}
	connector := c.app.Connector
	return c.json(append(place, httpx.Field{Key: "courses", Value: courses},
		httpx.Field{Key: "line", Value: quick.Available()}, httpx.Field{Key: "library", Value: data.Library != nil},
		httpx.Field{Key: "grades", Value: connector != nil && connector.Registry.Tool("add_grade") != nil}))
}

// addLine is {text}: what the model understood of one line; nothing is saved.
func addLine(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	line, _ := body["text"].(string)
	quick, err := c.quick()
	if err != nil {
		return err
	}
	understood, err := quick.Read(c.ctx(), line)
	var refused *QuickError
	if errors.As(err, &refused) {
		if known, ok := quickErrors[refused.Reason]; ok {
			fail(c.w, known.status, c.say(known.en, known.ru))
			return nil
		}
		fail(c.w, http.StatusBadGateway, c.say("The line was not read: "+refused.Reason,
			"Строку не разобрали: "+refused.Reason))
		return nil
	}
	if err != nil {
		return err
	}
	return c.json(understood)
}

func (c *call) libraryWired() (*library.Library, error) {
	data, err := c.data()
	if err != nil {
		return nil, err
	}
	if data.Library == nil {
		return nil, httpx.Fail(http.StatusServiceUnavailable, "the library is not wired")
	}
	return data.Library, nil
}

// placed is the library's course and class from the app's class uid or course key; the inbox without either.
func (c *call) placed(classUID, courseKey string) (library.Item, error) {
	if classUID != "" {
		lib, err := c.libraryWired()
		if err != nil {
			return library.Item{}, err
		}
		found, err := lib.ClassOf(classUID)
		if err != nil {
			return library.Item{}, err
		}
		if found == nil {
			return library.Item{}, httpx.BadRequest("no such class")
		}
		return library.Item{Course: found.Subject, ClassUID: classUID}, nil
	}
	if courseKey != "" {
		data, err := c.data()
		if err != nil {
			return library.Item{}, err
		}
		name, err := data.CourseName(courseKey)
		if err != nil {
			return library.Item{}, err
		}
		if name == "" {
			return library.Item{}, httpx.BadRequest("no such course")
		}
		return library.Item{Course: name}, nil
	}
	return library.Item{}, nil
}

// journalMaterial journals material added to the library, with the undo that removes it when remove_material is a
// tool: the change's {id, summary, undo}.
func (c *call) journalMaterial(ids []int64, what string) (httpx.Object, error) {
	marks := make([]string, len(ids))
	listed := make([]any, len(ids))
	for i, id := range ids {
		marks[i], listed[i] = fmt.Sprintf("#%d", id), id
	}
	summary := "Added " + what + " to the library: " + strings.Join(marks, ", ")
	connector := c.app.Connector
	if connector == nil {
		return httpx.Object{{Key: "id", Value: nil}, {Key: "summary", Value: summary}, {Key: "undo", Value: false}}, nil
	}
	var undoSteps []tools.Step
	if connector.Registry.Tool("remove_material") != nil {
		undoSteps = []tools.Step{}
		for _, id := range ids {
			undoSteps = append(undoSteps, tools.Step{Tool: "remove_material", Arguments: map[string]any{"id": id}})
		}
	}
	id, err := connector.Journal("add_material", map[string]any{"ids": listed}, summary, undoSteps, "", "web")
	if err != nil {
		return nil, err
	}
	return httpx.Object{{Key: "id", Value: id}, {Key: "summary", Value: summary},
		{Key: "undo", Value: undoSteps != nil}}, nil
}

// addFiles is multipart: `class_uid` or `course` (a key) first, then the files; each is written to disk as it comes
// and kept in the library (source web). A refused file stops the upload; the files before it stay, journaled.
func addFiles(c *call) error {
	lib, err := c.libraryWired()
	if err != nil {
		return err
	}
	reader, err := c.r.MultipartReader()
	if err != nil {
		fail(c.w, http.StatusBadRequest, "multipart/form-data expected")
		return nil
	}
	fields := map[string]string{}
	added := []int64{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(c.w, http.StatusBadRequest, "multipart/form-data expected")
			return nil
		}
		if part.FileName() == "" {
			name := part.FormName()
			if name != "class_uid" && name != "course" || len(added) > 0 {
				fail(c.w, http.StatusBadRequest, "class_uid and course come before the files")
				return nil
			}
			if fields[name], err = formField(part, FieldBytes); err != nil {
				return err
			}
			continue
		}
		place, err := c.placed(fields["class_uid"], fields["course"])
		if err != nil {
			return err
		}
		place.Source, place.Author = "web", "owner"
		id, err := lib.AddStream(part, part.FileName(), part.Header.Get("Content-Type"), "", place)
		var refused *library.Error
		if errors.As(err, &refused) {
			if len(added) == 0 {
				fail(c.w, http.StatusBadRequest, refused.Msg)
				return nil
			}
			changed, err := c.journalMaterial(added, "files")
			if err != nil {
				return err
			}
			httpx.JSON(c.w, http.StatusMultiStatus, httpx.Object{{Key: "added", Value: added},
				{Key: "change", Value: changed}, {Key: "error", Value: refused.Msg}})
			return nil
		}
		if err != nil {
			return err
		}
		added = append(added, id)
	}
	if len(added) == 0 {
		fail(c.w, http.StatusBadRequest, c.say("Choose a file.", "Выберите файл."))
		return nil
	}
	changed, err := c.journalMaterial(added, "files")
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "added", Value: added}, {Key: "change", Value: changed}})
}

// addNote is {text, class_uid?, course?}: a note for the library; a line that is only a link is kept as a link.
func addNote(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	text, ok := body["text"].(string)
	if !ok || pytext.Strip(text) == "" {
		fail(c.w, http.StatusBadRequest, c.say("Write the note.", "Напишите заметку."))
		return nil
	}
	text = pytext.Strip(text)
	classUID, _ := body["class_uid"].(string)
	courseKey, _ := body["course"].(string)
	place, err := c.placed(classUID, courseKey)
	if err != nil {
		return err
	}
	lib, err := c.libraryWired()
	if err != nil {
		return err
	}
	place.Source, place.Author = "web", "owner"
	var id int64
	if url := libfeed.FirstURL(text); url != "" && url == text {
		place.URL = &url
		id, err = lib.Add("link", place)
	} else {
		place.Text, place.Title = &text, pytext.Cut(spaced(text), 80)
		id, err = lib.Add("text", place)
	}
	var refused *library.Error
	if errors.As(err, &refused) {
		fail(c.w, http.StatusBadRequest, refused.Msg)
		return nil
	}
	if err != nil {
		return err
	}
	changed, err := c.journalMaterial([]int64{id}, "a note")
	if err != nil {
		return err
	}
	return c.json(httpx.Object{{Key: "added", Value: []int64{id}}, {Key: "change", Value: changed}})
}

// addByHand is POST /add/deadline {title, due, category?, course?}, /add/event {title, start, end?, room?, kind?},
// /add/grade {course: a key, label, value}: the connector's tools, journaled with their undo.
func addByHand(c *call) error {
	body, err := c.body()
	if err != nil {
		return err
	}
	connector, err := c.connector()
	if err != nil {
		return err
	}
	var tool string
	var arguments map[string]any
	switch c.r.PathValue("what") {
	case "deadline":
		category := body["category"]
		if !truthy(category) {
			category = "study"
		}
		tool, arguments = "add_deadline", map[string]any{"title": body["title"], "due": body["due"], "category": category}
		if truthy(body["course"]) {
			arguments["course"] = body["course"]
		}
	case "event":
		tool, arguments = "add_event", map[string]any{}
		for _, name := range []string{"title", "start", "end", "room", "kind"} {
			if truthy(body[name]) {
				arguments[name] = body[name]
			}
		}
	case "grade": // a grade's course is a short label, as a gradebook's: the lens's key is one
		tool, arguments = "add_grade", map[string]any{"course": body["course"], "label": body["label"],
			"value": body["value"]}
	default:
		fail(c.w, http.StatusNotFound, "no such thing to add")
		return nil
	}
	return c.act(connector, tool, arguments)
}
