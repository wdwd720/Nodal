// Command goroutine_bomb starts goroutines until something stops it.
package main

func main() {
	block := make(chan struct{})
	for {
		go func() { <-block }()
	}
}
