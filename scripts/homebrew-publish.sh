#!/usr/bin/env bash
# Commits a formula from scripts/homebrew-formula.sh to a Homebrew tap, unless
# the tap already has that version or a later one: a rerun for an older
# release must not move people back to it.
#
#   GH_TOKEN=... scripts/homebrew-publish.sh OWNER/REPO FORMULA
set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: $0 OWNER/REPO FORMULA" >&2
	exit 2
fi
repo=$1 formula=$2
path=Formula/$(basename "$formula")

# Homebrew reads a formula's version from its archives' names, and so does this.
formula_version() {
	sed -n 's#^ *url ".*/diderot_\(.*\)_darwin_arm64\.tar\.gz".*#\1#p'
}

new=$(formula_version <"$formula")
if [[ -z $new ]]; then
	echo "$0: $formula has no version" >&2
	exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
sha=""
if gh api "repos/$repo/contents/$path" >"$tmp/current.json" 2>"$tmp/err"; then
	sha=$(jq -r .sha "$tmp/current.json")
	old=$(jq -r .content "$tmp/current.json" | base64 -d | formula_version)
	newest=$(printf '%s\n%s\n' "$old" "$new" | sort -V | tail -n 1)
	if [[ $old == "$new" || $newest != "$new" ]]; then
		echo "$repo has diderot $old; not replacing it with $new"
		exit 0
	fi
elif ! grep -q "HTTP 404" "$tmp/err"; then
	cat "$tmp/err" >&2
	exit 1
fi

args=(--method PUT "repos/$repo/contents/$path"
	-f message="diderot $new"
	-f content="$(base64 <"$formula" | tr -d '\n')")
[[ -n $sha ]] && args+=(-f sha="$sha")
gh api "${args[@]}" --jq "\"Committed \\(.content.path) to $repo as \\(.commit.sha)\""
