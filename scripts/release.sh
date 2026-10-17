#!/bin/sh
# Cuts a release. Versions are YYMM.x - two digits of year, two of month, a
# dot, then the release number within that month (2610.1 = the first release of
# October 2026). Nothing is padded and nothing is derived from commits: the tag
# is the version, and it is the next free number in the current month.
#
#   ./scripts/release.sh          print the next tag, change nothing
#   ./scripts/release.sh --yes    create the tag and push it
#
# Pushing the tag starts the Release workflow (binaries + GitHub Release) and
# the Docker workflow (the :YYMM.x image).
set -eu

cd "$(dirname "$0")/.."

month=$(date +%y%m)
last=$(git tag --list "$month.*" | sed "s/^$month\.//" | sort -n | tail -1)
tag="$month.$(( ${last:-0} + 1 ))"

if [ "${1:-}" != "--yes" ]; then
	echo "$tag   (run \"$0 --yes\" to tag and push it)"
	exit 0
fi

if [ -n "$(git status --porcelain)" ]; then
	echo "error: the working tree is dirty - commit or stash first" >&2
	exit 1
fi

git tag -a "$tag" -m "Release $tag"
git push origin "$tag"
echo "tagged $tag and pushed it - the Release workflow is building the binaries"
