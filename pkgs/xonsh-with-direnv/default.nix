{
  lib,
  python3,
  xonsh-direnv,
}:
(python3.withPackages (
  ps:
    with ps; [
      xonsh
      xonsh-direnv
    ]
)).overrideAttrs
(old: {
  passthru =
    (old.passthru or {})
    // {
      shellPath = "/bin/xonsh";
    };

  meta =
    (old.meta or {})
    // {
      description = "Xonsh with direnv xontrib support";
      mainProgram = "xonsh";
      platforms = lib.platforms.unix;
    };
})
