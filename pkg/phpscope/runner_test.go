package phpscope

import "testing"

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://pyroscope:4040":             "http://pyroscope:4040",
		"https://user:secret@pyroscope:443": "https://user:xxxxx@pyroscope:443",
		"http://[::1%zz":                    "<invalid url>",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
