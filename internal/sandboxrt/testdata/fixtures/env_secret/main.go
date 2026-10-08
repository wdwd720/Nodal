// Command env_secret reports what it can see of the host: the secret the
// test planted in its own environment, the whole environment, and its
// arguments.
package main

import (
	"os"
	"strconv"
	"strings"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		secret, found := os.LookupEnv("NODAL_FAKE_SECRET")
		environ := strings.Join(os.Environ(), ";")
		if len(environ) > 200 {
			environ = environ[:200]
		}
		return s.Log("environment", map[string]string{
			"secret":        secret,
			"secret_found":  strconv.FormatBool(found),
			"environ_count": strconv.Itoa(len(os.Environ())),
			"environ":       environ,
			"args":          strings.Join(os.Args, " "),
		})
	})
}
