package okx

import "testing"

func Test50105Hint(t *testing.T) {
	e := &APIError{Code: "50105", Msg: "IP受限"}
	s := e.Error()
	if want := "IP 白名单"; !contains(s, want) {
		t.Fatalf("hint missing in %q", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
