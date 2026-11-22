package main

import (
	"io"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		argv    []string
		command string
		data    string
		tablo   string
	}{
		{nil, "run", "data", ""},
		{[]string{"--data", "other"}, "run", "other", ""},
		{[]string{"login", "--data", "other"}, "login", "other", ""}, // the command first, as the docs write it
		{[]string{"--data=other", "login"}, "login", "other", ""},
		{[]string{"import-tablo", "--tablo-db", "tablo.db"}, "import-tablo", "data", "tablo.db"},
	} {
		args, err := parse(tc.argv, io.Discard)
		if err != nil {
			t.Fatalf("%v: %v", tc.argv, err)
		}
		if args.command != tc.command || args.data != tc.data || args.tabloDB != tc.tablo {
			t.Fatalf("%v: %+v", tc.argv, args)
		}
	}
	if args, err := parse([]string{"--shadow", "--data", "copy"}, io.Discard); err != nil || !args.shadow ||
		args.command != "run" || args.data != "copy" {
		t.Fatalf("--shadow: %+v %v", args, err)
	}
	if args, _ := parse(nil, io.Discard); args.shadow {
		t.Fatal("the shadow mode is on by default")
	}
	for _, argv := range [][]string{{"serve"}, {"login", "run"}, {"--nope"}} {
		if _, err := parse(argv, io.Discard); err == nil {
			t.Fatalf("%v parsed", argv)
		}
	}
}

func TestLogLevel(t *testing.T) {
	for _, name := range []string{"DEBUG", "info", "WARNING", "ERROR"} {
		if _, err := logLevel(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := logLevel("LOUD"); err == nil {
		t.Fatal("LOUD is a level")
	}
}
