package main

import (
	"strings"
	"testing"
)

func TestFormatScreen(t *testing.T) {
	rule := strings.Repeat("─", 110)
	lines := []string{
		"", "",
		"⏺ Длинная строка, которую терминал",
		"перенёс сам",
		rule, rule,
		"❯ привет",
		rule,
		"  ⏵⏵ auto mode on" + strings.Repeat(" ", 70) + "✔ Update installed",
		"", "", "",
		"конец",
		"", "",
	}
	rows := make([]ScreenRow, len(lines))
	rows[2].TrailingSpaces = 1
	rows[3].Wrapped = true

	got := formatScreen(lines, rows, 0, 3500)
	want := strings.Join([]string{
		"⏺ Длинная строка, которую терминал перенёс сам",
		shortRule,
		"❯ привет",
		shortRule,
		"  ⏵⏵ auto mode on   ✔ Update installed",
		"",
		"конец",
	}, "\n")
	if got != want {
		t.Fatalf("got:\n%s\n\nwant:\n%s", got, want)
	}

	// Последние N строк и лимит по символам режут сверху по границе строки.
	if got := formatScreen(lines, rows, 2, 3500); got != "конец" {
		t.Fatalf("n=2: %q", got)
	}
	if got := formatScreen(lines, rows, 0, 12); got != "…\nконец" {
		t.Fatalf("maxRunes: %q", got)
	}
}
