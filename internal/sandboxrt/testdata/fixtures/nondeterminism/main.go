// Command nondeterminism reads every source of nondeterminism a Go program
// has -- the wall clock, the monotonic clock, two random sources, map
// iteration order, the process id -- and reports what it saw. In the sandbox
// every one of them is a function of the artifact and the input.
package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		start := time.Now()
		buf := make([]byte, 16)
		_, _ = crand.Read(buf)
		m := map[int]int{}
		for i := 0; i < 64; i++ {
			m[i] = i
		}
		var order []string
		for k := range m {
			order = append(order, strconv.Itoa(k))
		}
		time.Sleep(250 * time.Millisecond)
		host, _ := os.Hostname()
		return s.Log("observed", map[string]string{
			"now":         start.UTC().Format(time.RFC3339Nano),
			"elapsed":     time.Since(start).String(),
			"math_rand":   strconv.FormatUint(rand.Uint64(), 10),
			"crypto_rand": hex.EncodeToString(buf),
			"map_order":   strings.Join(order, ","),
			"pid":         strconv.Itoa(os.Getpid()),
			"hostname":    host,
			"as_of_seen":  s.AsOf(),
			"instruments": strconv.Itoa(len(s.Instruments())),
		})
	})
}
