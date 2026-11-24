# alert-agent

Triage agent for a k3s homelab. Alertmanager posts firing alerts to the
controller, which runs Claude Code headless (`claude -p`, on a Claude
subscription via `CLAUDE_CODE_OAUTH_TOKEN`) in a Kubernetes Job per incident.

- Low-risk, reversible fixes are applied through a fixed set of controller
  tools, each logged before it runs.
- Anything larger becomes a pull request against the cluster's GitOps repo,
  with a notification asking for review.
- A web UI shows incident history, transcripts, items awaiting review, and a
  chat that resumes the agent's session for a pending change.

The design and phase plan live in the cluster repo at
`docs/plans/alert-agent/`.

## Layout

One binary, one image, two subcommands:

- `alert-agent controller` -- webhook receiver, store, UI, MCP tool server
- `alert-agent runner` -- wraps `claude -p` inside a per-incident Job

## Development

```bash
make test   # go test -race ./...
make lint   # golangci-lint at the pinned version
make run    # controller on :8080
```
