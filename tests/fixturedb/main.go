// Command fixturedb initializes a temporary Go state database for Python tests.
package main

import (
	"fmt"
	"os"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fixturedb <state.db>")
		os.Exit(2)
	}
	s, err := store.Open(os.Args[1])
	if err == nil {
		err = s.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
