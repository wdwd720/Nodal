// Command momentum is a well-behaved strategy: for every instrument it
// compares a short and a long simple moving average of the closes and
// proposes buying when the short one is above the long one.
package main

import (
	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		units, ok := s.Config("order_units")
		if !ok {
			units = "1000000"
		}
		for _, ins := range s.Instruments() {
			if len(ins.Closes) < 5 {
				continue
			}
			short, err := nodalsdk.SMA(ins.Closes, 3, 6, nodalsdk.RoundHalfEven)
			if err != nil {
				return err
			}
			long, err := nodalsdk.SMA(ins.Closes, 5, 6, nodalsdk.RoundHalfEven)
			if err != nil {
				return err
			}
			signal := "flat"
			if short.Cmp(long) > 0 {
				signal = "long"
				if err := s.EmitIntent("ACQUIRE_EXPOSURE", ins.ID, "BUY", units, ""); err != nil {
					return err
				}
			}
			if err := s.Log("signal", map[string]string{
				"instrument": ins.ID, "short": short.String(), "long": long.String(), "signal": signal,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
