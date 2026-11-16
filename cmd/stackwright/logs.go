package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"strconv"

	"connectrpc.com/connect"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
)

// runLogs prints a run's timeline; with -follow, until the run ends.
func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	tf := addTargetFlags(fs)
	follow := fs.Bool("follow", false, "go on until the run ends")
	_ = fs.Parse(args)
	a, err := signedIn()
	if err != nil {
		return err
	}
	ctx := context.Background()
	t, err := a.find(ctx, tf)
	if err != nil {
		return err
	}
	var number int32
	switch fs.NArg() {
	case 0:
		out, err := a.runs.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{Org: t.org, Project: t.project, Workspace: t.workspace}))
		if err != nil {
			return err
		}
		if len(out.Msg.GetRuns()) == 0 {
			return errors.New("the workspace has no runs")
		}
		number = out.Msg.GetRuns()[0].GetNumber()
	case 1:
		n, err := strconv.ParseInt(fs.Arg(0), 10, 32)
		if err != nil {
			return errors.New("name a run by its number")
		}
		number = int32(n)
	default:
		return errors.New("usage: stackwright logs [-follow] [number]")
	}
	_, err = a.follow(ctx, os.Stdout, t, number, *follow)
	return err
}
