// Command signing_attempt tries, in the mode its configuration names, to get
// something other than a typed proposal out of the sandbox: a signing
// request, a transfer, a withdrawal, a destination smuggled into an intent,
// or an intent of a kind a strategy may never propose. It bypasses the SDK
// and writes raw bytes, as hostile code would.
package main

import (
	"os"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		mode, _ := s.Config("mode")
		instrument := ""
		if ins := s.Instruments(); len(ins) > 0 {
			instrument = ins[0].ID
		}
		var line string
		switch mode {
		case "sign_record":
			line = `{"type":"sign","seq":1,"transaction":"AQABAgMEBQYH","signer":"hot-wallet"}`
		case "transfer_record":
			line = `{"type":"transfer","seq":1,"to_account":"00000000-0000-7000-8000-000000000001","quantity":"1000000"}`
		case "withdraw_record":
			line = `{"type":"withdraw","seq":1,"destination":"So11111111111111111111111111111111111111112","quantity":"1000000"}`
		case "intent_with_destination":
			line = `{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + instrument +
				`","side":"BUY","quantity":"1","destination":"So11111111111111111111111111111111111111112"}`
		case "intent_with_signature":
			line = `{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + instrument +
				`","side":"BUY","quantity":"1","signature":"deadbeef"}`
		case "withdraw_kind":
			line = `{"type":"intent","seq":1,"kind":"WITHDRAW","instrument_id":"` + instrument + `","side":"SELL","quantity":"1000000"}`
		case "transfer_kind":
			line = `{"type":"intent","seq":1,"kind":"TRANSFER_VALUE","instrument_id":"` + instrument + `","side":"SELL","quantity":"1000000"}`
		case "bridge_kind":
			line = `{"type":"intent","seq":1,"kind":"BRIDGE_VALUE","instrument_id":"` + instrument + `","side":"SELL","quantity":"1000000"}`
		default:
			line = `{"type":"log","seq":1,"event":"idle"}`
		}
		_, err := os.Stdout.Write([]byte(line + "\n"))
		return err
	})
}
