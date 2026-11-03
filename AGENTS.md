# Repository Instructions

This repository is public. Do not add sensitive infrastructure details, secret
names, private host bootstrap material, operator runbooks, hardware
fingerprints, recovery procedures, or security-model trade-offs here.

Use `nix-packages` only for reusable public packages and generic tooling. Put
private configuration, private defaults, and sensitive operational notes in the
private `nix-secrets` flake instead.

CI on the shared runners runs cheap checks only and never builds packages
(ADR-0003). Before pushing a package change, build it locally as the README's
"Local builds before pushing" section describes.
