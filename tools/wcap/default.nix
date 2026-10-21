{
  lib,
  buildGoModule,
  makeWrapper,
  gpu-screen-recorder,
  fuzzel,
  libnotify,
  pipewire,
  pulseaudio,
  writableTmpDirAsHomeHook,
}:

buildGoModule {
  pname = "wcap";
  version = "0.1.0";

  src = ./.;

  vendorHash = null; # no external dependencies

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603).
  nativeBuildInputs = [
    writableTmpDirAsHomeHook
    makeWrapper
  ];

  postInstall = ''
    wrapProgram $out/bin/wcap \
      --prefix PATH : ${
        lib.makeBinPath [
          gpu-screen-recorder
          fuzzel
          libnotify
          pipewire # pw-loopback
          pulseaudio # pactl
        ]
      }
  '';

  meta = {
    description = "Lightweight window capture with PipeWire audio isolation";
    mainProgram = "wcap";
  };
}
