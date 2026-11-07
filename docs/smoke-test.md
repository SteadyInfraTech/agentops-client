# Smoke test: both operators and the client on a real cluster

Nothing in these three repositories has yet been run end to end. This is the checklist for the first
run. It is written from the repositories' Makefiles and READMEs, not from a run, so fix it where the
real behaviour differs.

You need a cluster, `kubectl`, the `kustomize` CLI, a container registry, and a Cloudflare account
with a zone you control (API token with Tunnel, DNS and Access edit permissions).

## 1. Install the operators

**ai-agent-edge-tunnel** (see its README for the namespace-scoped variant):

```bash
kubectl create namespace edge-tunnel-system
kubectl -n edge-tunnel-system create secret generic edge-tunnel-cloudflare \
  --from-literal=account-id=... --from-literal=api-token=...
kubectl -n edge-tunnel-system create configmap edge-tunnel-config \
  --from-literal=EDGE_BASE_DOMAIN=agents.example.com \
  --from-literal=CONNECTOR_IMAGE=<pinned cloudflared image>
make docker-build IMG=<registry>/ai-agent-edge-tunnel:<tag>   # push it too
make deploy IMG=<registry>/ai-agent-edge-tunnel:<tag>
```

**agentops-operator**:

```bash
make docker-build IMG=<registry>/agentops-operator:<tag>      # push it too
make install                                                  # CRDs
make deploy IMG=<registry>/agentops-operator:<tag>
```

Then create an agent the sessions can bind to (`make docker-build-agent` builds a reference echo
agent). Use the AgentRuntime, AgentService and AgentPool samples in `config/samples/`; the pool's
selector label is `agents.steadyinfra.com/pool`. Check the pool has a ready replica before going on.

## 2. Expose a session

```bash
go run ./examples/smoke -namespace <ns> -name smoke -pool <pool> \
  -zone-id <cloudflare zone id> -email you@example.com
```

The command blocks and keeps the session open. Leave it running in this terminal and run the checks
below from a second one. Press Ctrl-C in the first terminal only after they finish: it completes the
session and starts tunnel teardown.

Expected, in order:

1. The program prints the session UID.
2. `kubectl -n <ns> get agentsession smoke -o yaml` reaches `Bound` and has `status.endpoint`.
3. `kubectl -n <ns> get agentedgetunnel smoke -o yaml` shows `status.sessionUID` and `status.sessionEndpoint`
   recorded, then `phase: Active` and a connector that is ready.
4. The program prints `https://smoke.<EDGE_BASE_DOMAIN>`. Opening it asks for a Cloudflare Access
   login; only the allowed email gets through. The reference echo agent does not serve `GET /`, so
   a 404 after login is expected. To see the agent answer, send `POST /v1/chat` with
   `{"message": "hello"}` as the logged-in user, for example with `fetch` from the browser console
   on that page.

## 3. Things worth checking on purpose

- **Ending the session ends the tunnel.** Press Ctrl-C (or `kubectl -n <ns> patch agentsession smoke --type
  merge -p '{"spec":{"completed":true}}'`). Within about 10 seconds plus reconcile time the tunnel
  goes `Terminated`, and the connector Deployment, DNS record, Access application and Cloudflare tunnel
  are gone. Look in the Cloudflare dashboard, not only in the cluster.
- **Deleting the session** does the same, and the tunnel also has an ownerReference to it.
- **Order independence.** Apply the tunnel before the session (with `kubectl`, not the client); it
  should wait, not fail.
- **Public by omission is refused.** An `AgentEdgeTunnel` with neither `allowedEmails` nor
  `allowPublic` must be rejected by the API server.
- **Re-binding terminates.** If the session's `status.endpoint` changes, the tunnel ends instead of
  being re-pointed.
- **Leaks.** After all of the above, list tunnels, DNS records and Access applications in Cloudflare
  and confirm nothing from the test remains. Known gap: a crash between creating a Cloudflare resource
  and recording its ID can orphan it (edge-tunnel issue #4).

## 4. Clean up

```bash
kubectl -n <ns> delete agentsession smoke --ignore-not-found
make undeploy        # in each operator repository; the edge one also removes its AgentEdgeTunnels
```
