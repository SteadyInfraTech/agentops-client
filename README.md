# agentops-client

A small Go library for creating an `AgentSession` (from
[agentops-operator](https://github.com/SteadyInfraTech/agentops-operator)) together with the
`AgentEdgeTunnel` that exposes it (from
[ai-agent-edge-tunnel](https://github.com/SteadyInfraTech/ai-agent-edge-tunnel)), so every caller
does not repeat the same steps.

It depends on neither operator. Both kinds are handled as `unstructured`, so the two projects keep
their own versions and release cycles; the fields used are the contract described in
agentops-operator's `docs/design/edge-exposure.md`.
