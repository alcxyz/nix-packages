# Repository Instructions

This repository is public. Do not add sensitive infrastructure details, secret
names, private host bootstrap material, operator runbooks, hardware
fingerprints, recovery procedures, or security-model trade-offs here.

Use `nix-packages` only for reusable public packages and generic tooling. Put
private configuration, private defaults, and sensitive operational notes in the
private `nix-secrets` flake instead.
