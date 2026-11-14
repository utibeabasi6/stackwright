package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
)

// runRuns lists a workspace's newest runs.
func runRuns(args []string) error {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	tf := addTargetFlags(fs)
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
	out, err := a.runs.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{Org: t.org, Project: t.project, Workspace: t.workspace}))
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s/%s\n", t.org, t.project, t.workspace)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "#\tSTATUS\tCHANGES\tFROM\tSTARTED")
	for _, r := range out.Msg.GetRuns() {
		_, _ = fmt.Fprintf(w, "%d\t%s\t+%d ~%d -%d\t%s\t%s\n", r.GetNumber(), status(r.GetStatus()), r.GetAdds(), r.GetChanges(), r.GetDestroys(),
			r.GetSource(), r.GetCreatedAt().AsTime().Local().Format(time.DateTime))
	}
	return w.Flush()
}

func status(s v1.RunStatus) string {
	return map[v1.RunStatus]string{
		v1.RunStatus_RUN_STATUS_QUEUED: "queued", v1.RunStatus_RUN_STATUS_PLANNING: "planning", v1.RunStatus_RUN_STATUS_NEEDS_APPROVAL: "needs approval",
		v1.RunStatus_RUN_STATUS_APPLYING: "applying", v1.RunStatus_RUN_STATUS_PLANNED: "planned", v1.RunStatus_RUN_STATUS_APPLIED: "applied",
		v1.RunStatus_RUN_STATUS_FAILED: "failed", v1.RunStatus_RUN_STATUS_REJECTED: "rejected", v1.RunStatus_RUN_STATUS_CANCELED: "canceled",
		v1.RunStatus_RUN_STATUS_BLOCKED: "blocked",
	}[s]
}
