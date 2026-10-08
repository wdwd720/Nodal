// Command greedy asks for more than any reasonable manifest allows: a kind it
// was not granted, an instrument it was not granted, a notional above the
// cap and more intents than a run may have. It is well-formed throughout, so
// only the manifest stands in its way.
package main

import (
	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		ins := s.Instruments()
		if len(ins) < 2 {
			return nil
		}
		a, b := ins[0].ID, ins[1].ID
		steps := []struct{ kind, instrument, side, qty string }{
			{"ACQUIRE_EXPOSURE", a, "BUY", "1000000"},  // within the manifest
			{"OPEN_POSITION", a, "BUY", "1000000"},     // a kind it was not granted
			{"ACQUIRE_EXPOSURE", b, "BUY", "1"},        // an instrument it was not granted
			{"ACQUIRE_EXPOSURE", a, "BUY", "10000000"}, // above the notional cap
			{"ACQUIRE_EXPOSURE", a, "BUY", "1000000"},  // within the manifest
			{"ACQUIRE_EXPOSURE", a, "BUY", "1000000"},  // one more than a run may have
		}
		for _, st := range steps {
			if err := s.EmitIntent(st.kind, st.instrument, st.side, st.qty, ""); err != nil {
				return err
			}
		}
		return nil
	})
}
