#!/usr/bin/env bash
# Writes the Homebrew formula for one release of the diderot client to stdout.
#
#   scripts/homebrew-formula.sh TAG CHECKSUMS URL_BASE
#
# TAG is the release tag (v1.2.3), CHECKSUMS the release's checksums.txt, and
# URL_BASE where its archives are.
#
# A formula rather than a cask, which is what GoReleaser would write: Homebrew
# quarantines what a cask downloads, so Gatekeeper stops the unsigned binary,
# and never quarantines what a formula downloads.
set -euo pipefail

if [[ $# -ne 3 ]]; then
	echo "usage: $0 TAG CHECKSUMS URL_BASE" >&2
	exit 2
fi
tag=$1 checksums=$2 base=${3%/}
version=${tag#v}
# A release tag, or a snapshot's (v1.2.3-SNAPSHOT-abc1234) for CI's test install.
if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "$0: $tag is not a release tag (vX.Y.Z)" >&2
	exit 2
fi

# Homebrew reads a release's version from its archives' names (and its audit
# objects to stating it); it cannot read a snapshot's.
version_line=""
[[ $version == *-* ]] && version_line="  version \"$version\""

archive() {
	local name="diderot_${version}_$1.tar.gz" sum
	sum=$(awk -v f="$name" '$2 == f { print $1 }' "$checksums")
	if [[ ! $sum =~ ^[0-9a-f]{64}$ ]]; then
		echo "$0: no sha256 for $name in $checksums" >&2
		exit 1
	fi
	printf '      url "%s/%s"\n      sha256 "%s"\n' "$base" "$name" "$sum"
}

darwin_arm64=$(archive darwin_arm64)
darwin_amd64=$(archive darwin_amd64)
linux_arm64=$(archive linux_arm64)
linux_amd64=$(archive linux_amd64)

echo "# Written by bluesky-social/diderot's release workflow (scripts/homebrew-formula.sh). Do not edit."
cat <<EOF
class Diderot < Formula
  desc "Ask a Diderot server, and set up coding agents to use it"
  homepage "https://github.com/bluesky-social/diderot"
EOF
[[ -n $version_line ]] && echo "$version_line"
cat <<EOF
  license any_of: ["MIT", "Apache-2.0"]

  livecheck do
    skip "Each release publishes this formula"
  end

  on_macos do
    on_arm do
$darwin_arm64
    end
    on_intel do
$darwin_amd64
    end
  end

  on_linux do
    on_arm do
$linux_arm64
    end
    on_intel do
$linux_amd64
    end
  end

  def install
    bin.install "diderot"
    # Homebrew installs README.md and the LICENSE-* files itself.
    doc.install "third-party"
  end

  def caveats
    <<~EOS
      To sign in to your Diderot server and set up your coding agents:
        diderot setup <server URL>
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/diderot --version")
  end
end
EOF
