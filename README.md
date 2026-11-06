# agentops-client

A small Go library for creating an `AgentSession` (from
[agentops-operator](https://github.com/SteadyInfraTech/agentops-operator)) together with the
`AgentEdgeTunnel` that exposes it (from
[ai-agent-edge-tunnel](https://github.com/SteadyInfraTech/ai-agent-edge-tunnel)), so every caller
does not repeat the same steps.

It depends on neither operator. Both kinds are handled as `unstructured`, so the two projects keep
their own versions and release cycles; the fields used are the contract described in
agentops-operator's `docs/design/edge-exposure.md`.

## Usage

```go
import agentops "github.com/SteadyInfraTech/agentops-client"

ex, err := agentops.Expose(ctx, c, agentops.ExposeRequest{
    Namespace: "agents", Name: "session-42",
    PoolName:  "support-agents",
    ZoneID:    "<cloudflare zone id>", Subdomain: "session-42",
    AllowedEmails: []string{"user@example.com"}, // or AllowPublic: true
})
if err != nil { /* nothing was left behind */ }

ep, err := agentops.Wait(ctx, c, ex, agentops.WaitOptions{Timeout: 2 * time.Minute})
// ep.URL is https://<hostname>, reachable once the tunnel is Active and its
// connector is connected.

// When the user is done:
err = agentops.Complete(ctx, c, ex)
```

`Expose` creates the `AgentSession`, then an `AgentEdgeTunnel` with `spec.sessionRef` and an
`ownerReference` to the session. If the tunnel cannot be created the session is deleted again.
`Wait` keeps waiting through `Failed` (the edge operator retries) and stops early only on
`Terminated`. The client `c` is any controller-runtime client with access to both kinds.

Both operators must be installed; this library depends on neither and only handles the objects as
unstructured. The tunnel exposes the agent's Service, not one session, so it gives no per-session isolation.
