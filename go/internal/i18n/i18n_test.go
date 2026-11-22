package i18n

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestPhrase(t *testing.T) {
	phrase := P("the file is gone", "файла нет")
	if phrase.String() != "the file is gone" || fmt.Sprintf("%s", phrase) != "the file is gone" { // the log speaks English
		t.Fatal(phrase)
	}
	if phrase.Get("ru") != "файла нет" || phrase.Get("en") != "the file is gone" {
		t.Fatal(phrase)
	}
	// say(): a Phrase in the language, a plain text as it is
	if Localize(phrase, "ru") != "файла нет" || Localize("as it is", "ru") != "as it is" {
		t.Fatal("Localize")
	}
	if Tr("ru", "yes", "да") != "да" || Tr("en", "yes", "да") != "yes" {
		t.Fatal("Tr")
	}
	if !AlertP("down", "не работает").Alert || phrase.Alert {
		t.Fatal("Alert")
	}
}

func TestReasonsAndJSON(t *testing.T) {
	if Reason(Err("no code", "нет кода"), "ru") != "нет кода" {
		t.Fatal("Reason of a Phrase")
	}
	if Reason(fmt.Errorf("wrapped: %w", Err("no code", "нет кода")), "ru") != "нет кода" {
		t.Fatal("Reason of a wrapped Phrase")
	}
	if Reason(errors.New("plain"), "ru") != "plain" || Reason(errors.New(""), "ru") != "" {
		t.Fatal("Reason of a plain error")
	}
	data := map[string]any{"error": P("the LLM limit", "лимит LLM"), "ok_at": nil}
	text, err := json.Marshal(Localize(data, "ru"))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	json.Unmarshal(text, &back)
	if back["error"] != "лимит LLM" || back["ok_at"] != nil || len(back) != 2 {
		t.Fatal(string(text))
	}
	if _, err := json.Marshal(Localize(map[string]any{"x": make(chan int)}, "en")); err == nil {
		t.Fatal("anything but a Phrase is still not JSON")
	}
	// Without Localize a Phrase writes its English (str() of the Python service).
	if text, _ := json.Marshal(map[string]any{"e": P("en", "ru")}); string(text) != `{"e":"en"}` {
		t.Fatal(string(text))
	}
}
