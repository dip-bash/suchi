// Command suchi is a clipboard history and snippet manager for the terminal.
package main

import (
	"os"

	"github.com/dip-bash/suchi/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
