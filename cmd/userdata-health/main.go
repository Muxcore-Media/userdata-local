// userdata-health runs separately from the daemon, using only existing identity
// files. It never initializes storage, enrolls, or contacts core/auth-local.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Muxcore-Media/userdata-local/httpclient"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "userdata health probe failed:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("userdata-health", flag.ContinueOnError)
	origin := flags.String("origin", "", "userdata HTTP(S) origin; defaults to local USERDATA_LOCAL_HTTP_ADDR")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	cfg, err := httpclient.FromEnv(*origin, "userdata-local")
	if err != nil {
		return err
	}
	if cfg.Origin == "" {
		addr := os.Getenv("USERDATA_LOCAL_HTTP_ADDR")
		if addr == "" {
			addr = ":9701"
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("invalid USERDATA_LOCAL_HTTP_ADDR")
		}
		// Only this local probe translates a wildcard bind into loopback.
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		if host == "::" {
			host = "::1"
		}
		scheme := "https"
		if cfg.Insecure {
			scheme = "http"
		}
		cfg.Origin = scheme + "://" + net.JoinHostPort(host, port)
	}
	cfg.Timeout = 3 * time.Second
	client, err := httpclient.New(cfg)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, httpclient.GetHealth, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider health status %d", resp.StatusCode)
	}
	return nil
}
