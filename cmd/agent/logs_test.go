package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogsLimit(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{nil, 100},
		{[]string{"logs"}, 100},
		{[]string{"logs", "250"}, 250},
		{[]string{"logs", "0"}, 100},
		{[]string{"logs", "99999"}, 10000},
	}
	for _, tc := range cases {
		got := parseLogsLimit(tc.args)
		if got != tc.want {
			t.Fatalf("parseLogsLimit(%v)=%d want %d", tc.args, got, tc.want)
		}
	}
}

func TestTailFileLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	content := []string{"one", "two", "three", "four", "five"}
	if err := os.WriteFile(path, []byte(strings.Join(content, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tailFileLines(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "three,four,five" {
		t.Fatalf("unexpected tail: %#v", lines)
	}
}
