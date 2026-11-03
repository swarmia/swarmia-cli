package main

import (
	"github.com/swarmia/swarmia-cli/cmd"
	"github.com/swarmia/swarmia-cli/internal/execpath"
)

func main() {
	execpath.Fix()
	cmd.Execute()
}
