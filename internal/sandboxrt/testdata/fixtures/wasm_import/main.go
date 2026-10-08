// Command wasm_import declares a host function that would sign a
// transaction. No such host function exists; the runtime refuses the module
// before it starts.
package main

//go:wasmimport nodal sign_transaction
func signTransaction(ptr, length uint32) uint32

func main() {
	buf := []byte("transfer everything")
	_ = signTransaction(0, uint32(len(buf)))
}
