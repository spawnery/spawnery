# The rendered documentation site. A narrow source set, so a code change does
# not rebuild it; docs/superpowers/ is left out because mkdocs.yml excludes
# it anyway.
{ lib
, stdenvNoCC
, python3Packages
, mermaid-js
, docs-fonts
, agent-api-javadoc
}:

stdenvNoCC.mkDerivation {
  pname = "spawnery-docs-site";
  version = "0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.difference
      (lib.fileset.unions [ ../docs ../mkdocs.yml ../overrides ])
      ../docs/superpowers;
  };

  nativeBuildInputs = with python3Packages; [
    mkdocs
    mkdocs-material
    mkdocs-mermaid2-plugin
  ];

  # Gitignored, so absent from the source set: mermaid.js, the fonts and the
  # Javadoc tree, which mkdocs copies through as static files.
  postPatch = ''
    mkdir -p docs/assets
    install -m 644 ${mermaid-js}/mermaid.min.js docs/assets/mermaid.min.js

    mkdir -p docs/assets/fonts
    install -m 644 ${docs-fonts}/*.woff2 docs/assets/fonts/

    mkdir -p docs/plugin-api/javadoc
    cp -r ${agent-api-javadoc}/. docs/plugin-api/javadoc/
  '';

  # --strict is the project's only link checker.
  buildPhase = ''
    runHook preBuild
    export HOME="$TMPDIR"
    mkdocs build --strict --site-dir "$out"
    runHook postBuild
  '';

  dontInstall = true;
}
