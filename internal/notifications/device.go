package notifications

import "strings"

// What a "new sign-in" notification is allowed to say about the sign-in.
//
// The follower copied the login's IP address and User-Agent header straight out
// of `security_events` into `notifications.data`, and those two tables have
// opposite retention properties. `security_events` is partitioned by month
// precisely so a month can be DROPped (00740), and `cmd/api`'s retention pass
// does exactly that. `notifications` refuses DELETE from every role including
// the owner (NOTIFICATION_IMMUTABLE), and nothing anywhere purges it. So a
// person's address and their browser fingerprint were copied from the place
// built to forget them into the place that cannot (F-188).
//
// The notification keeps what makes "was this me?" answerable at a glance -- a
// coarse locality and the browser -- and points at `/v1/me/audit`, which serves
// the exact address from the trail that CAN be dropped. Nothing is lost; it is
// read from the copy that has a retention policy.

// deviceSummary reduces a User-Agent header to the two facts a person uses to
// recognise their own sign-in: the browser and the platform.
//
// It is deliberately coarse. A full User-Agent is a fingerprint -- version
// numbers, build ids, device models -- and this string is stored forever, so
// what it may carry is the minimum that answers the question. An unrecognised
// agent produces the empty string rather than a guess or a truncated header:
// the exact value is one link away and inventing a label for it would be worse
// than saying nothing.
func deviceSummary(userAgent string) string {
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return ""
	}
	browser := matchFirst(ua, []labelled{
		// Order matters and is the whole subtlety: Edge and Opera both claim
		// Chrome, and Chrome claims Safari. The most specific token wins.
		{"Edg", "Edge"},
		{"OPR", "Opera"},
		{"Firefox", "Firefox"},
		{"Chrome", "Chrome"},
		{"Safari", "Safari"},
	})
	platform := matchFirst(ua, []labelled{
		{"iPhone", "iPhone"},
		{"iPad", "iPad"},
		{"Android", "Android"},
		{"CrOS", "ChromeOS"},
		{"Windows", "Windows"},
		{"Mac OS X", "macOS"},
		{"Macintosh", "macOS"},
		{"Linux", "Linux"},
	})
	switch {
	case browser != "" && platform != "":
		return browser + " on " + platform
	case browser != "":
		return browser
	default:
		return platform
	}
}

type labelled struct{ token, label string }

func matchFirst(s string, in []labelled) string {
	for _, l := range in {
		if strings.Contains(s, l.token) {
			return l.label
		}
	}
	return ""
}

// newSessionBody is the copy. It names the locality and the device when they
// are known, says nothing invented when they are not, and always says where the
// exact address is.
func newSessionBody(device, ipPrefix string) string {
	var b strings.Builder
	b.WriteString("A new session was created for your account")
	switch {
	case device != "" && ipPrefix != "":
		b.WriteString(" from " + device + ", on the network " + ipPrefix)
	case device != "":
		b.WriteString(" from " + device)
	case ipPrefix != "":
		b.WriteString(" from the network " + ipPrefix)
	}
	b.WriteString(". If this was not you, sign out every session from Settings and sign in again. ")
	b.WriteString("Your security history shows the exact address this sign-in came from.")
	return b.String()
}
