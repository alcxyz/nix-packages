**History of** [ADR-0006](../0006-runtime-configured-nix-deploy.md): text replaced on 2026-10-31.

# ADR-0006: Runtime-configured nix-deploy

## Text replaced on 2026-10-31 (phase 3)

The `triage` row of the inventory table, before deploy read its
`agentCommand` to offer triage (ADR-0016 phase 3):

| `triage` | object, optional | Read only by `nixfleet triage` (ADR-0016), which validates only this object, so an error elsewhere in the inventory does not stop it, and an inventory it cannot read only skips the launch. `agentCommand`: non-empty array of non-empty strings, the agent CLI's argv, to which triage appends one prompt argument; `inputCheckouts`: object of flake input name to the local checkout of that operator-owned input; `playbookAddendum`: path of a private Markdown playbook addendum. All three are optional; paths may start with `~/` and must be absolute after that; other fields are rejected. Missing checkouts or addendum only warn; leave the object out to write the context without launching an agent |
