// Command panicker emits a valid proposal and then panics: a run that does
// not end cleanly yields nothing, including what it wrote before failing.
package main

import nodalsdk "nodal.dev/sdk"

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		for _, ins := range s.Instruments() {
			if err := s.EmitIntent("ACQUIRE_EXPOSURE", ins.ID, "BUY", "1", ""); err != nil {
				return err
			}
		}
		panic("strategy gave up")
	})
}
