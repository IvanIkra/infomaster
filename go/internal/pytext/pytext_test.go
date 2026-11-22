package pytext

import "testing"

func TestLikePython(t *testing.T) {
	if got := Casefold("Straße ΣΑΣ"); got != "strasse σασ" {
		t.Errorf("Casefold = %q", got)
	}
	if got := Split(" a\x1cb c\n"); len(got) != 3 || got[1] != "b" {
		t.Errorf("Split = %q", got)
	}
	if got := Quote("a b/ж?=&", "/"); got != "a%20b/%D0%B6%3F%3D%26" {
		t.Errorf("Quote = %q", got)
	}
	if got := Quote("a/b", ""); got != "a%2Fb" {
		t.Errorf("Quote safe='' = %q", got)
	}
	if Len("жук") != 3 || Cut("жук", 2) != "жу" || Cut("ab", 5) != "ab" {
		t.Error("Len/Cut")
	}
}
