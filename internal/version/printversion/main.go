// Command printversion prints what version.Get reports. The version tests build
// it with -ldflags, the way the Dockerfile builds the manager.
package main

import (
	"fmt"

	"github.com/sserkanml/backflow/internal/version"
)

func main() {
	v, c := version.Get()
	fmt.Println(v, c)
}
