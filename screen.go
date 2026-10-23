package main

// Подготовка экрана терминала для телефона. Терминал широкий (100+ колонок),
// а в Telegram на телефоне помещается около 40, поэтому:
//   - строки, которые терминал перенёс сам, склеиваются обратно — пусть их переносит Telegram;
//   - разделители во всю ширину (рамка ввода Claude Code и т. п.) становятся короткими;
//   - длинные пробелы внутри строки (выравнивание по правому краю) сжимаются.

import "strings"

// ScreenRow — метаданные строки экрана из `terminal read --screen` (поле tailRows).
type ScreenRow struct {
	// Wrapped — строка продолжает предыдущую (мягкий перенос терминала).
	Wrapped bool `json:"wrapped"`
	// TrailingSpaces — сколько пробелов Orca срезала в конце строки.
	TrailingSpaces int `json:"trailingSpaces"`
}

const (
	shortRule    = "────────────"
	minRuleChars = 8 // строка из стольких символов-линий считается разделителем
	maxGap       = 5 // пробелы внутри строки длиннее этого сжимаются
	maxIndent    = 6 // больший отступ слева урезается
)

func isRuleChar(r rune) bool {
	switch r {
	case '─', '━', '═', '-', '—', '–', '‒', '_', '╌', '╍', '┄', '┅', '┈', '┉', '▔', '▁', '=':
		return true
	}
	return false
}

func isRule(s string) bool {
	n := 0
	for _, r := range s {
		switch {
		case r == ' ':
		case isRuleChar(r):
			n++
		default:
			return false
		}
	}
	return n >= minRuleChars
}

// tidyLine убирает лишние пробелы: урезает большой отступ и сжимает длинные промежутки.
func tidyLine(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	s = strings.TrimRight(s, " \t")
	body := strings.TrimLeft(s, " ")
	indent := len(s) - len(body)
	if indent > maxIndent {
		indent = maxIndent
	}
	var sb strings.Builder
	sb.WriteString(strings.Repeat(" ", indent))
	spaces := 0
	for _, r := range body {
		if r == ' ' {
			spaces++
			continue
		}
		if spaces > 0 {
			if spaces > maxGap {
				spaces = 3
			}
			sb.WriteString(strings.Repeat(" ", spaces))
			spaces = 0
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// formatScreen готовит экран к отправке: последние n логических строк (0 — все),
// не длиннее maxRunes символов; обрезка — по границе строки, сверху.
func formatScreen(lines []string, rows []ScreenRow, n, maxRunes int) string {
	// 1. Склеиваем мягкие переносы.
	var logical []string
	for i, l := range lines {
		if i > 0 && i < len(rows) && rows[i].Wrapped && len(logical) > 0 {
			prev := logical[len(logical)-1]
			if rows[i-1].TrailingSpaces > 0 {
				prev += " "
			}
			logical[len(logical)-1] = prev + l
			continue
		}
		logical = append(logical, l)
	}

	// 2. Чистим строки, сжимаем разделители и пустые строки.
	var out []string
	lastRule, lastBlank := false, true // true — не начинаем с пустой строки
	for _, l := range logical {
		t := tidyLine(l)
		switch {
		case isRule(t):
			if lastRule {
				continue
			}
			out = append(out, shortRule)
			lastRule, lastBlank = true, false
		case t == "":
			if lastBlank {
				continue
			}
			out = append(out, "")
			lastBlank = true
		default:
			out = append(out, t)
			lastRule, lastBlank = false, false
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}

	// 3. Хвост: n строк и не больше maxRunes символов.
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	total := 0
	start := len(out)
	for start > 0 {
		l := len([]rune(out[start-1])) + 1
		if total+l > maxRunes {
			break
		}
		total += l
		start--
	}
	if start == len(out) && len(out) > 0 {
		// Даже последняя строка не влезает — режем её саму.
		r := []rune(out[len(out)-1])
		return "…" + string(r[len(r)-maxRunes:])
	}
	for start < len(out) && out[start] == "" {
		start++
	}
	res := strings.Join(out[start:], "\n")
	if start > 0 {
		res = "…\n" + res
	}
	return res
}
