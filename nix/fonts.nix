# The documentation site's two typefaces, served by the site itself, latin
# and latin-ext subsets only.
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
