package adminplane

import (
	"encoding/json"
	"strconv"
	"time"
)

// Seconds is a duration that crosses the wire as a whole number of seconds.
//
// It exists because time.Duration marshals as nanoseconds, and a console that
// reads a field named "..._seconds" and gets 900000000000 will render a step-up
// window of 28,000 years or, worse, silently treat it as milliseconds. Whole
// seconds are also exactly representable in a JavaScript number, which no
// nanosecond count above 2^53 is.
//
// Sub-second remainders are truncated toward zero. Every duration this package
// exports is a policy window measured in minutes or hours, so truncation never
// loses anything real; a value that did need sub-second precision would not
// belong in this type.
type Seconds time.Duration

// Sec converts a duration for export.
func Sec(d time.Duration) Seconds { return Seconds(d) }

// Duration returns the underlying duration.
func (s Seconds) Duration() time.Duration { return time.Duration(s) }

// Int returns the whole seconds.
func (s Seconds) Int() int64 { return int64(time.Duration(s) / time.Second) }

// MarshalJSON writes the whole seconds as a JSON number.
func (s Seconds) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(s.Int(), 10)), nil
}

// UnmarshalJSON reads a JSON number of whole seconds.
func (s *Seconds) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*s = Seconds(time.Duration(n) * time.Second)
	return nil
}
