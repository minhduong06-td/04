package web

import (
	"net/http"
	"testing"
)

func TestItoaProducesValidRetryAfterValue(t *testing.T) {
	for _, n := range []int{0, 1, 30, 3600} {
		value := itoa(n)
		header := http.Header{}
		header.Set("Retry-After", value)
		if got := header.Get("Retry-After"); got != value {
			t.Fatalf("itoa(%d) produced an invalid header value %q", n, value)
		}
		for _, b := range []byte(value) {
			if b < '0' || b > '9' {
				t.Fatalf("itoa(%d) contains non-digit byte %#x in %q", n, b, value)
			}
		}
	}
}
