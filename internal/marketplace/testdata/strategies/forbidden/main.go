// Command forbidden reads a file. The scanner refuses it before it is built.
package main

import (
	"os"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		_, err := os.ReadFile("/etc/passwd")
		return err
	})
}
