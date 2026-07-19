package server

import (
	"reflect"
	"testing"
)

func TestParsePorcelainUnstaged(t *testing.T) {
	in := "" +
		" M internal/server/server.go\n" +
		"M  staged_only.go\n" +
		"?? newfile.txt\n" +
		" D deleted.go\n" +
		"R  old.go -> new.go\n"
	got := parsePorcelainUnstaged(in)
	want := []GitStatusEntry{
		{Path: "internal/server/server.go", Status: "modified"},
		{Path: "newfile.txt", Status: "untracked"},
		{Path: "deleted.go", Status: "deleted"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
