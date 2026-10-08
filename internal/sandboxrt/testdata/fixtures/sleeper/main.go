// Command sleeper sleeps for an hour. On the sandbox's virtual clock the
// hour passes at once and costs nothing.
package main

import (
	"time"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		before := time.Now()
		time.Sleep(time.Hour)
		after := time.Now()
		return s.Log("slept", map[string]string{
			"before": before.UTC().Format(time.RFC3339Nano),
			"after":  after.UTC().Format(time.RFC3339Nano),
			"slept":  after.Sub(before).Truncate(time.Second).String(),
		})
	})
}
