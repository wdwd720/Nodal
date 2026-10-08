// Command memory_bomb allocates and keeps memory until something stops it.
package main

func main() {
	var keep [][]byte
	for {
		b := make([]byte, 1<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		keep = append(keep, b)
	}
}
