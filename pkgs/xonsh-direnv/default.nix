{
  lib,
  python3Packages,
  fetchPypi,
}:
python3Packages.buildPythonPackage rec {
  pname = "xonsh-direnv";
  version = "1.6.5";
  format = "setuptools";

  src = fetchPypi {
    pname = "xonsh_direnv";
    inherit version;
    hash = "sha256-kWTR62EWW2dwWqC9neXS/JsCyxufoBrAabLj7xn4hYQ=";
  };

  meta = {
    description = "direnv support for the xonsh shell";
    homepage = "https://github.com/74th/xonsh-direnv";
    license = lib.licenses.mit;
    maintainers = [];
  };
}
