{
  lib,
  calibre,
  python3Packages,
  python314Packages ? null,
  speechd-minimal,
}:
let
  # Calibre 9 uses Python 3.14; older recipes use the default Python set.
  pythonPackages =
    if (lib.functionArgs calibre.override) ? python314Packages then
      python314Packages
    else
      python3Packages;

  speechd = (speechd-minimal.override { python3Packages = pythonPackages; }).overrideAttrs (old: {
    # configure executes Python to detect its bindings. Under strictDeps a
    # target buildInput alone is invisible, silently omitting the modules.
    nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ pythonPackages.python ];
    configureFlags = (old.configureFlags or [ ]) ++ [ "--enable-python" ];
  });

  withBindings = calibre.override { speechd-minimal = speechd; };
in
withBindings.overrideAttrs (
  old:
  {
    # 8.4 predates the fixture locks added upstream; newer sources already
    # have them, but their optimistic freshness check still bypasses the lock.
    patches = (old.patches or [ ]) ++ [
      (if old.version == "8.4.0" then ./fixture-lock-8.4.patch else ./fixture-lock.patch)
    ];
    postInstallCheck = (old.postInstallCheck or "") + ''
      python ${./test-fixture-lock.py} src/calibre/ebooks/oeb/polish/tests/base.py
    '';
  }
  //
    lib.optionalAttrs
      (lib.any (input: lib.getName input == "speech-dispatcher") (old.buildInputs or [ ]))
      {
        # With strictDeps, native Python's setup hook does not expose target
        # buildInputs. Both install checks and installed launchers need the path.
        preInstallCheck = (old.preInstallCheck or "") + ''
          export PYTHONPATH="${speechd}/${pythonPackages.python.sitePackages}''${PYTHONPATH:+:$PYTHONPATH}"
        '';
        preFixup = ''
          gappsWrapperArgs+=(--prefix PYTHONPATH : ${speechd}/${pythonPackages.python.sitePackages})
        ''
        + (old.preFixup or "");
      }
)
