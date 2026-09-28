package httpapi

import "testing"

func TestParseCookieStringFormats(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"plain pairs", "a=1; b=2; c=3", 3},
		{"full header", "Cookie: xiaomichatbot_serviceToken=tok; userId=42", 2},
		{"quoted values", `Cookie: "xiaomichatbot_serviceToken"="tok"; "userId"="42"`, 2},
		{"trailing semicolon", "a=1; b=2;", 2},
		{"messy whitespace", "  a = 1 ;;  b = 2  ", 2},
		{"empty", "", 0},
		{"no pairs", "garbage", 0},
		{"missing value", "a=; b=2", 1},
		{"duplicates deduped", "a=1; a=2", 1},
		{"real world", "xiaomichatbot_serviceToken=abc123==; userId=99; xiaomichatbot_ph=n3%2Fxyz==; deviceId=wb_1", 4},
	}
	for _, c := range cases {
		got := ParseCookieString(c.in)
		if len(got) != c.want {
			t.Errorf("%s: got %d cookies, want %d (%+v)", c.name, len(got), c.want, got)
		}
	}
}

func TestParseCookieStringPreservesValue(t *testing.T) {
	// Base64-ish values contain = which must not be treated as a separator.
	got := ParseCookieString("xiaomichatbot_serviceToken=EXAMPLEphValue0000==")
	if len(got) != 1 {
		t.Fatalf("got %d cookies", len(got))
	}
	if got[0].Value != "EXAMPLEphValue0000==" {
		t.Errorf("value = %q, trailing = was truncated", got[0].Value)
	}
}
