package main

import (
	"slices"
	"testing"
)

func TestStringListTakesCommaSeparatedValuesAndCanBeRepeated(t *testing.T) {
	var l stringList
	for _, v := range []string{"gitlab.com, github.com", "", " git.example.com ,,*.corp.example.com"} {
		if err := l.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"gitlab.com", "github.com", "git.example.com", "*.corp.example.com"}
	if !slices.Equal(l.values, want) {
		t.Errorf("values = %v, want %v", l.values, want)
	}
	if l.String() != "gitlab.com,github.com,git.example.com,*.corp.example.com" {
		t.Errorf("String() = %q", l.String())
	}
}
