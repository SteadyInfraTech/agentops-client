// Command smoke exposes one agent session through Cloudflare and prints the
// URL, for checking both operators together on a real cluster. See
// docs/smoke-test.md. It ends the session on exit unless -keep is set.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	agentops "github.com/SteadyInfraTech/agentops-client"
)

func main() {
	var (
		ns      = flag.String("namespace", "default", "namespace for both objects")
		name    = flag.String("name", "smoke", "name of the session and tunnel, also used as the subdomain")
		pool    = flag.String("pool", "", "AgentPool name (set exactly one of -pool and -agent-service)")
		svc     = flag.String("agent-service", "", "AgentService name")
		zone    = flag.String("zone-id", "", "Cloudflare zone ID")
		email   = flag.String("email", "", "email allowed through Cloudflare Access")
		timeout = flag.Duration("timeout", 3*time.Minute, "how long to wait for the endpoint")
		keep    = flag.Bool("keep", false, "leave the session running on exit")
	)
	flag.Parse()
	if *email == "" || *zone == "" {
		fmt.Fprintln(os.Stderr, "error: -email and -zone-id are required")
		os.Exit(2)
	}
	if err := run(*ns, *name, *pool, *svc, *zone, *email, *timeout, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run exposes the session, waits for the endpoint and, unless keep is set,
// ends the session when interrupted.
func run(ns, name, pool, svc, zone, email string, timeout time.Duration, keep bool) error {
	cfg, err := config.GetConfig()
	if err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: runtime.NewScheme()})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ex, err := agentops.Expose(ctx, c, agentops.ExposeRequest{
		Namespace: ns, Name: name, PoolName: pool, AgentServiceName: svc,
		ZoneID: zone, Subdomain: strings.ToLower(name), AllowedEmails: []string{email},
	})
	if err != nil {
		return err
	}
	fmt.Printf("created session and tunnel %s/%s (session uid %s)\n", ns, name, ex.SessionUID)

	cleanup := func(cause error) error {
		if keep {
			return cause
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if cerr := agentops.Complete(cctx, c, ex); cerr != nil {
			return errors.Join(cause, fmt.Errorf("completing session %s (uid %s): %w", name, ex.SessionUID, cerr))
		}
		return cause
	}

	ep, err := agentops.Wait(ctx, c, ex, agentops.WaitOptions{Timeout: timeout})
	if err != nil {
		return cleanup(err)
	}
	fmt.Println("endpoint:", ep.URL)
	if keep {
		return nil
	}
	fmt.Println("press Ctrl-C to end the session")
	<-ctx.Done()
	return cleanup(nil)
}
