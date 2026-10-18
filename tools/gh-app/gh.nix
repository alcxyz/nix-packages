# A gh that routes through gh-app, for tools that take a gh package. Without
# the App's settings in the environment it runs plain gh unchanged.
{
  gh,
  gh-app,
  makeWrapper,
  runCommand,
}:

runCommand "gh-app-gh-${gh-app.version}"
  {
    nativeBuildInputs = [ makeWrapper ];
    meta = {
      description = "gh routed through gh-app's GitHub App token for the App's installed repositories";
      mainProgram = "gh";
    };
  }
  ''
    makeWrapper ${gh-app}/bin/gh-app "$out/bin/gh" \
      --add-flags gh \
      --set-default GH_APP_GH ${gh}/bin/gh
  ''
