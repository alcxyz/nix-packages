{
  lib,
  piper-tts,
  python3Packages,
}:
# Older nixpkgs releases package the C++ implementation without Python inputs.
if (lib.functionArgs piper-tts.override) ? python3Packages then
  piper-tts.override {
    python3Packages = python3Packages.overrideScope (
      _final: prev: {
        pyopenjtalk-plus = prev.pyopenjtalk-plus.overridePythonAttrs (
          old:
          lib.optionalAttrs (old.version == "0.4.1-post9") {
            # Cython can generate either TypeError wording. Keep the exception
            # type check and the test's subsequent successful-call assertion.
            postPatch = (old.postPatch or "") + ''
              substituteInPlace tests/test_frontend.py \
                --replace-fail 'match="must be str"' 'match="must be str|Expected str"'
            '';
          }
        );
      }
    );
  }
else
  piper-tts
