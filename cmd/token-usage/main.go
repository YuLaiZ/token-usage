package main

import (
	"os"

	"github.com/YuLaiZ/token-usage/internal/cli"
)

func main() {
	os.Exit(cli.ExecuteWithConsole(cli.NewRootCmd()))
}
