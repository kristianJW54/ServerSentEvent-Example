package main

import (
	"fmt"
	"runtime"
)

func main() {

	fmt.Println("number of goroutines:", runtime.NumGoroutine()) // Should be 1 now
}
