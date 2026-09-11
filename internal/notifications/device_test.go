package notifications

import "testing"

// A device summary is two facts and no fingerprint. The cases that matter are
// the ones where a header claims to be several browsers at once: Edge and Opera
// both say Chrome, and Chrome says Safari, so "contains" alone answers wrongly
// for three of the five.
func TestDeviceSummary_NamesTheBrowserAndThePlatformAndNothingElse(t *testing.T) {
	for _, tc := range []struct{ ua, want string }{
		{"", ""},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36", "Chrome on Windows"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0", "Edge on Windows"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15", "Safari on macOS"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0", "Firefox on Linux"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1", "Safari on iPhone"},
		{"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/141.0.0.0 Mobile Safari/537.36 OPR/76.0", "Opera on Android"},
		{"curl/8.5.0", ""},
	} {
		if got := deviceSummary(tc.ua); got != tc.want {
			t.Errorf("deviceSummary(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}

	// No version number, build id or device model survives into the summary:
	// this string is written into a row nothing can ever delete.
	full := "Mozilla/5.0 (Linux; Android 14; SM-S918B Build/UP1A.231005.007) AppleWebKit/537.36 Chrome/141.0.6779.135 Mobile Safari/537.36"
	if got := deviceSummary(full); got != "Chrome on Android" {
		t.Errorf("deviceSummary kept more than the browser and the platform: %q", got)
	}
}

// The copy says where the exact address is, whatever it knows.
func TestNewSessionBody_AlwaysSaysWhereTheExactAddressIs(t *testing.T) {
	for _, tc := range []struct{ device, prefix string }{
		{"Chrome on Windows", "203.0.113.0/24"},
		{"Chrome on Windows", ""},
		{"", "203.0.113.0/24"},
		{"", ""},
	} {
		body := newSessionBody(tc.device, tc.prefix)
		if len(body) == 0 {
			t.Fatal("empty body")
		}
		for _, want := range []string{"exact address", "sign out every session"} {
			if !contains(body, want) {
				t.Errorf("newSessionBody(%q, %q) = %q, missing %q", tc.device, tc.prefix, body, want)
			}
		}
		if tc.prefix != "" && !contains(body, tc.prefix) {
			t.Errorf("newSessionBody(%q, %q) does not name the network", tc.device, tc.prefix)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
