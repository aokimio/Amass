package main

import (
	"bytes"
	"flag"
)

func RunHelpCommand(clArgs []string) {
	help := []string{"-help"}
	helpBuf := new(bytes.Buffer)
	helpCommand := flag.NewFlagSet("help", flag.ContinueOnError)
	helpCommand.SetOutput(helpBuf)
	if len(clArgs) < 1 {
		CommandUsage(mainUsageMsg, helpCommand, helpBuf)
		return
	}
	switch clArgs[0] {
	case "db":
		RunDBCommand(help)
	case "enum":
		RunEnumCommand(help)
	case "intel":
		RunIntelCommand(help)
	case "track":
		RunTrackCommand(help)
	case "viz":
		RunVizCommand(help)
	default:
		CommandUsage(mainUsageMsg, helpCommand, helpBuf)
		return
	}
}
