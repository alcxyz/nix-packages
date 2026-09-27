{ k3s, callPackage }:
let
  sourcePatch = ./flannel-node-source.patch;
  patched =
    (k3s.override {
      overrideBundleAttrs = old: {
        patches = (old.patches or [ ]) ++ [ sourcePatch ];
        preCheck = (old.preCheck or "") + ''
          go test -count=1 ./pkg/agent/flannel
        '';
      };
    }).overrideAttrs
      (old: {
        # The bundle builds k3s-server; the outer derivation also builds the CLI.
        patches = (old.patches or [ ]) ++ [ sourcePatch ];
        passthru = (old.passthru or { }) // {
          tests = ((old.passthru or { }).tests or { }) // {
            flannel-source-vm = callPackage ./vm-test.nix { k3s = patched; };
          };
        };
      });
in
patched
