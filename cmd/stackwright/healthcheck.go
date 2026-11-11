package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"time"
)

func runHealthcheck(args []string) error {
	fl := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	url := fl.String("url", "http://127.0.0.1:8080/healthz", "health URL")
	_ = fl.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", *url, resp.Status)
	}
	return nil
}
