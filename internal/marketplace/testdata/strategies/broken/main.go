// Command broken passes the scanner and does not compile.
package main

import (
	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		var units int = s.AsOf()
		_ = units
		return nil
	})
}
