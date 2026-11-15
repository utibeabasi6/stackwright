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
  rotate-kek   wrap every data key with STACKWRIGHT_KEK instead of STACKWRIGHT_KEK_PREVIOUS
  healthcheck  exit 0 if the server answers /healthz

  login        sign in to a Stackwright server with a code you approve in the browser
  plan         upload the working tree and plan it; nothing applies it
  runs         list a workspace's runs
  logs         print a run's timeline; -follow waits until it ends

plan, runs and logs find the project from the git remote and the folder;
-org, -project and -workspace name it instead.`

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
	case "rotate-kek":
		err = runRotateKEK(os.Args[2:])
	case "user":
		err = runUser(os.Args[2:])
	case "healthcheck":
		err = runHealthcheck(os.Args[2:])
	case "login":
		err = runLogin(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "runs":
		err = runRuns(os.Args[2:])
	case "logs":
		err = runLogs(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stackwright:", err)
		os.Exit(1)
	}
}
