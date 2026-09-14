# mermaid.min.js for the documentation site.
#
# Both loaders reach for unpkg on their own -- mkdocs-material's theme bundle
# and mkdocs-mermaid2-plugin, independently -- and a site served from one's own
# cluster should not depend on somebody else's.
#
# From npm rather than from nixpkgs' mermaid-cli, whose copy of this file is
# byte-identical and whose closure is 2.2 GiB: it carries Chromium for the
# headless rendering nothing here does.
{ fetchurl
, runCommand
}:

let
  version = "11.16.0";

  tarball = fetchurl {
    url = "https://registry.npmjs.org/mermaid/-/mermaid-${version}.tgz";
    hash = "sha256-/0jJSgoEWLN3pRh60BQHGE0qGC5kdsIBW3Bo/1g1X64=";
  };
in
runCommand "mermaid-${version}-min-js" { } ''
  mkdir -p $out
  tar -xzOf ${tarball} package/dist/mermaid.min.js > $out/mermaid.min.js
''
