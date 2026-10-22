{ khal }:
khal.overrideAttrs (_: {
  # sphinxHook expects a shell word list. With structured attributes, a Nix
  # list becomes a Bash array and the hook reads only its first element.
  sphinxBuilders = "html man";
})
