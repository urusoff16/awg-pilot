// Package i18n shows the program's messages in English when the user's language is not Russian.
// Messages are written in Russian in the code; T translates a finished message (an error, a log
// line, a notification) on its way to the screen, by exact text or by the format string it was
// made from, translating the formatted arguments too (wrapped errors are messages themselves).
package i18n

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

var english atomic.Bool

// SetLanguage switches the messages: "ru" keeps Russian, anything else is English.
func SetLanguage(lang string) { english.Store(lang != "ru") }

// English reports whether messages are shown in English.
func English() bool { return english.Load() }

// Lang is "ru" or "en".
func Lang() string {
	if English() {
		return "en"
	}
	return "ru"
}

// Resolve turns the setting ("", "ru", "en") into a language, using the system language for "".
// Only Russian systems get Russian; every other language gets English.
func Resolve(setting, system string) string {
	switch setting {
	case "ru", "en":
		return setting
	}
	if strings.HasPrefix(strings.ToLower(system), "ru") {
		return "ru"
	}
	return "en"
}

type pattern struct {
	re  *regexp.Regexp
	out string // English format with every verb turned into %s (indexes kept)
	n   int    // number of arguments
}

var (
	once     sync.Once
	exact    map[string]string
	patterns []pattern
	verbRe   = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[vsdqwfx]`)
	fixups   = []struct {
		re  *regexp.Regexp
		out string
	}{
		{regexp.MustCompile(`(\d) Б\b`), "$1 B"},
		{regexp.MustCompile(`(\d) КБ`), "$1 KB"},
		{regexp.MustCompile(`(\d) МБ`), "$1 MB"},
		{regexp.MustCompile(`(\d) ГБ`), "$1 GB"},
		{regexp.MustCompile(`(B|KB|MB|GB)/с`), "$1/s"},
		{regexp.MustCompile(`(\d\d\.\d\d\.\d{4}) в (\d\d:\d\d)`), "$1 $2"},
	}
)

func build() {
	exact = map[string]string{}
	keys := make([]string, 0, len(table))
	for ru, en := range table {
		if !verbRe.MatchString(ru) {
			exact[ru] = en
			continue
		}
		keys = append(keys, ru)
	}
	// Longer formats first: they are more specific than short ones like "%s: %s".
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, ru := range keys {
		parts := verbRe.Split(ru, -1)
		var b strings.Builder
		b.WriteString("^")
		for i, p := range parts {
			b.WriteString(regexp.QuoteMeta(p))
			if i < len(parts)-1 {
				b.WriteString("(.*?)")
			}
		}
		b.WriteString("$")
		out := verbRe.ReplaceAllString(table[ru], "%${1}s")
		patterns = append(patterns, pattern{re: regexp.MustCompile("(?s)" + b.String()), out: out, n: len(parts) - 1})
	}
}

// T returns s in the current language.
func T(s string) string {
	if !English() || s == "" {
		return s
	}
	once.Do(build)
	return fix(translate(s, 0))
}

func translate(s string, depth int) string {
	if depth > 6 || !hasCyrillic(s) {
		return s
	}
	if en, ok := exact[s]; ok {
		return en
	}
	for _, p := range patterns {
		m := p.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		args := make([]any, p.n)
		for i := range args {
			args[i] = translate(m[i+1], depth+1)
		}
		return sprintf(p.out, args)
	}
	// A log line "15:04:05 text" or "15:04:05 служба: text": keep the time, translate the rest.
	if len(s) > 9 && s[2] == ':' && s[5] == ':' && s[8] == ' ' {
		return s[:9] + translate(s[9:], depth+1)
	}
	return s
}

// sprintf fills a format whose verbs are all %s or %[n]s.
func sprintf(format string, args []any) string {
	var b strings.Builder
	next := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' || i+1 >= len(format) {
			b.WriteByte(c)
			continue
		}
		if format[i+1] == '%' {
			b.WriteByte('%')
			i++
			continue
		}
		idx := next
		j := i + 1
		if format[j] == '[' {
			k := strings.IndexByte(format[j:], ']')
			if k > 0 {
				n := 0
				for _, d := range format[j+1 : j+k] {
					n = n*10 + int(d-'0')
				}
				idx = n - 1
				j += k + 1
			}
		}
		if j < len(format) && format[j] == 's' {
			if idx >= 0 && idx < len(args) {
				b.WriteString(args[idx].(string))
			}
			next = idx + 1
			i = j
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func fix(s string) string {
	for _, f := range fixups {
		s = f.re.ReplaceAllString(s, f.out)
	}
	return s
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 0x400 && r <= 0x4FF {
			return true
		}
	}
	return false
}
