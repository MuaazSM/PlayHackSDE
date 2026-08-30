// Command racedemo fires N concurrent booking requests at one facility and one
// slot, then prints the outcome split. Exactly one must win.
//
// Placeholder: the target endpoint POST /api/v1/bookings does not exist until
// M1. The flag is wired so `make race-demo N=500` is correct the moment the
// write path lands.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	n := flag.Int("n", 500, "number of concurrent booking requests to fire")
	target := flag.String("target", "http://localhost:8080", "api base url")
	flag.Parse()

	fmt.Fprintf(os.Stderr,
		"racedemo: would fire n=%d at %s — POST /api/v1/bookings does not exist yet (lands in M1)\n",
		*n, *target)
}
