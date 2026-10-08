// Command output_flood writes valid log records to stdout forever.
package main

import (
	"os"
	"strconv"
)

func main() {
	for i := 1; ; i++ {
		line := `{"type":"log","seq":` + strconv.Itoa(i) + `,"event":"spam","fields":{"pad":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}` + "\n"
		_, _ = os.Stdout.Write([]byte(line))
	}
}
