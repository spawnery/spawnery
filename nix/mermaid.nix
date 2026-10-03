# mermaid.min.js, served by the site itself: mkdocs-material and the mermaid2
# plugin would each load it from unpkg. From npm, because nixpkgs' copy comes
# with mermaid-cli's 2.2 GiB closure.
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
