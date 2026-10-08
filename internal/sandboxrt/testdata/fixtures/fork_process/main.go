// Command fork_process tries to start processes and reports each outcome.
package main

import (
	"os"
	"os/exec"
	"strconv"

	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		attempt := func(name string, err error) error {
			msg := ""
			if err != nil {
				msg = err.Error()
				if len(msg) > 200 {
					msg = msg[:200]
				}
			}
			return s.Log("attempt", map[string]string{"name": name, "ok": strconv.FormatBool(err == nil), "error": msg})
		}
		p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", "echo pwned"}, &os.ProcAttr{})
		if err == nil {
			_, _ = p.Wait()
		}
		if e := attempt("start_process", err); e != nil {
			return e
		}
		err = exec.Command("/bin/sh", "-c", "echo pwned").Run()
		if e := attempt("exec_command", err); e != nil {
			return e
		}
		_, err = exec.LookPath("sh")
		return attempt("look_path", err)
	})
}
