# The two typefaces of the documentation site, served by the site itself.
#
# From Fontsource's npm tarballs for the reason nix/mermaid.nix gives: a site
# served from one's own cluster should not depend on somebody else's. Only the
# latin and latin-ext subsets, which is what the site's text uses.
{ fetchurl
, runCommand
}:

let
  jetbrainsMono = fetchurl {
    url = "https://registry.npmjs.org/@fontsource-variable/jetbrains-mono/-/jetbrains-mono-5.3.0.tgz";
    hash = "sha256-mW/mNopIDJzhXU3iKiaCt8QLQDcY/uKxXnJy8kT9mT8=";
  };

  ibmPlexSans = fetchurl {
    url = "https://registry.npmjs.org/@fontsource/ibm-plex-sans/-/ibm-plex-sans-5.3.0.tgz";
    hash = "sha256-IyWSryCuQTAmzHBfycEEE1+3YEMv1xDwagLDngYtMhM=";
  };
in
runCommand "docs-fonts" { } ''
  mkdir -p $out
  tar -xzf ${jetbrainsMono} -C $out --strip-components=2 \
    package/files/jetbrains-mono-latin-wght-normal.woff2 \
    package/files/jetbrains-mono-latin-wght-italic.woff2 \
    package/files/jetbrains-mono-latin-ext-wght-normal.woff2 \
    package/files/jetbrains-mono-latin-ext-wght-italic.woff2
  tar -xzf ${ibmPlexSans} -C $out --strip-components=2 \
    package/files/ibm-plex-sans-latin-400-normal.woff2 \
    package/files/ibm-plex-sans-latin-400-italic.woff2 \
    package/files/ibm-plex-sans-latin-500-normal.woff2 \
    package/files/ibm-plex-sans-latin-600-normal.woff2 \
    package/files/ibm-plex-sans-latin-ext-400-normal.woff2 \
    package/files/ibm-plex-sans-latin-ext-400-italic.woff2 \
    package/files/ibm-plex-sans-latin-ext-500-normal.woff2 \
    package/files/ibm-plex-sans-latin-ext-600-normal.woff2
''
