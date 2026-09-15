# The rendered documentation site.
#
# The source set is narrow deliberately. Every image derivation in this tree
# takes the whole working tree as its src, so a line changed under internal/
# moves all three hashes; this one reads docs/ and mkdocs.yml alone, which is
# what keeps `make image-repro` from rebuilding a game image because a
# sentence moved.
#
# docs/superpowers/ is excluded from that too: mkdocs.yml's exclude_docs
# already keeps its 88 dated specs and plans out of the built nav, so leaving
# them in the source set would only let editing one rebuild a site whose
# content does not change.
{ lib
, stdenvNoCC
, python3Packages
, mermaid-js
, agent-api-javadoc
}:

stdenvNoCC.mkDerivation {
  pname = "spawnery-docs-site";
  version = "0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.difference
      (lib.fileset.unions [ ../docs ../mkdocs.yml ])
      ../docs/superpowers;
  };

  nativeBuildInputs = with python3Packages; [
    mkdocs
    mkdocs-material
    mkdocs-mermaid2-plugin
  ];

  # docs/assets/mermaid.min.js is gitignored -- hack/vendor-mermaid.sh writes
  # it there for `mkdocs serve` -- so the source set above never carries it,
  # even though mkdocs.yml points the mermaid2 plugin at exactly that path.
  #
  # The Javadoc tree goes in beside it, for the same reason: mkdocs treats a
  # non-Markdown file under docs/ as a static asset it copies through
  # unparsed, so the nav entry in mkdocs.yml resolves under --strict as long
  # as the tree is here before `mkdocs build` runs, and not built from the
  # heavier agents.nix machinery -- see nix/agent-api-javadoc.nix.
  postPatch = ''
    mkdir -p docs/assets
    install -m 644 ${mermaid-js}/mermaid.min.js docs/assets/mermaid.min.js

    mkdir -p docs/plugin-api/javadoc
    cp -r ${agent-api-javadoc}/. docs/plugin-api/javadoc/
  '';

  # --strict turns an unresolved internal link and a page missing from the nav
  # into a build failure. It is the only link checker this project has.
  buildPhase = ''
    runHook preBuild
    export HOME="$TMPDIR"
    mkdocs build --strict --site-dir "$out"
    runHook postBuild
  '';

  dontInstall = true;
}
