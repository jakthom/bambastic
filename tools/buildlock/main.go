// Command buildlock runs a just recipe under a checkout-wide lock so that npm ci,
// Vite, and Go's asset embedding can finish before another build changes their inputs.
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/gofrs/flock"
)

func run() error {
	if err := os.MkdirAll("bin", 0o755); err != nil {
		return err
	}
	lock := flock.New("bin/.build.lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Close()

	recipe := exec.Command("just", os.Args[1:]...)
	recipe.Stdin, recipe.Stdout, recipe.Stderr = os.Stdin, os.Stdout, os.Stderr
	return recipe.Run()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
