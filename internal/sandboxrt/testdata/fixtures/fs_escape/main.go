// Command fs_escape tries every way it can think of to reach the host
// filesystem and reports each outcome as a log record. A sandbox that works
// makes every one of them fail.
package main

import (
	"os"
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
		_, err := os.ReadFile("/etc/passwd")
		if e := attempt("read_etc_passwd", err); e != nil {
			return e
		}
		_, err = os.ReadDir("/")
		if e := attempt("list_root", err); e != nil {
			return e
		}
		_, err = os.ReadDir(".")
		if e := attempt("list_cwd", err); e != nil {
			return e
		}
		_, err = os.ReadFile("../../../../../../etc/hosts")
		if e := attempt("relative_traversal", err); e != nil {
			return e
		}
		err = os.WriteFile("/tmp/nodal-sandbox-escape", []byte("pwned"), 0o600)
		if e := attempt("write_tmp", err); e != nil {
			return e
		}
		// The working directory is the Go runtime's own bookkeeping inside
		// the module, not a host directory: report it, then show it leads
		// nowhere.
		wd, _ := os.Getwd()
		if e := s.Log("cwd", map[string]string{"wd": wd}); e != nil {
			return e
		}
		_, err = os.ReadDir(wd)
		if e := attempt("list_getwd", err); e != nil {
			return e
		}
		_, err = os.Stat("/")
		if e := attempt("stat_root", err); e != nil {
			return e
		}
		f := os.NewFile(3, "preopen")
		_, err = f.Stat()
		return attempt("fd3_preopen", err)
	})
}
