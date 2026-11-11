package main

import (
	"fmt"
	"os"
)

const usage = `usage: stackwright <command> [flags]

commands:
  init         add any missing secrets to .env
  migrate      apply database migrations and set the app login
  server       run the control plane
  user         reset-password -email <address>
  healthcheck  exit 0 if the server answers /healthz`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = runInit(os.Args[2:])
	case "migrate":
		err = runMigrate(os.Args[2:])
	case "server":
		err = runServer(os.Args[2:])
	case "user":
		err = runUser(os.Args[2:])
	case "healthcheck":
		err = runHealthcheck(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stackwright:", err)
		os.Exit(1)
	}
}
