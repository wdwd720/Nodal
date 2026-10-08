// Command memory_huge asks for half a gibibyte at once.
package main

func main() {
	b := make([]byte, 512<<20)
	b[len(b)-1] = 1
	println(len(b))
}
