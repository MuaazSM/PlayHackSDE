// Command worker runs the async workers: outbox dispatcher, waitlist sweeper,
// no-show release. They can also be embedded in the api binary for the demo.
//
// Placeholder: no workers are registered yet. The outbox dispatcher lands with
// the write path (M1/M3), the sweepers with the waitlist (M4).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "worker: no workers registered yet (outbox dispatcher lands in M1/M3, sweepers in M4)")
}
