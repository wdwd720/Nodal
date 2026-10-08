// Command cpu_loop never returns and never calls into the host.
package main

func main() {
	n := 0
	for {
		n++
		if n < 0 {
			n = 0
		}
	}
}
