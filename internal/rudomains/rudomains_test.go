package rudomains

import "testing"

func TestMatch(t *testing.T) {
	m := NewMatcher(List([]string{"Госуслуги.рф", "https://Example.com/path", "*.foo.org"}))
	for name, want := range map[string]bool{
		"gosuslugi.ru": true, "www.sberbank.ru.": true, "vk.com": true, "sun6-21.userapi.com": true,
		"xn--c1aapkosapc.xn--p1ai": true, "example.com": true, "a.foo.org": true,
		"google.com": false, "ru.wikipedia.org": false, "notvk.com": false, "": false,
	} {
		if got := m.Match(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}
