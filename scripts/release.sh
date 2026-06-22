#!/usr/bin/env bash
# cctl local release — semantic versioning + Keep a Changelog, no CI.
#
# Computes the next version from the Conventional Commit subjects since the
# last release (feat → minor, fix/perf → patch, <type>! or "BREAKING CHANGE"
# → major), updates version.txt + CHANGELOG.md, commits "chore(release): vX",
# and tags vX. It does NOT push or build artifacts — it prints the push
# command at the end.
#
# Usage:
#   scripts/release.sh [--dry-run]
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

dry=0
[ "${1:-}" = "--dry-run" ] && dry=1

if [ -n "$(git status --porcelain)" ]; then
	echo "release: working tree is not clean — commit or stash first" >&2
	exit 1
fi

cur="$(tr -d '[:space:]' < version.txt 2>/dev/null || true)"
cur="${cur:-0.0.0}"

# Range of commits to consider: since the tag matching the current version,
# else the whole history.
base="v$cur"
if git rev-parse -q --verify "refs/tags/$base" >/dev/null 2>&1; then
	range="$base..HEAD"
else
	range="HEAD"
fi

subjects="$(git log $range --no-merges --format='%s')"
bodies="$(git log $range --format='%B')"

if [ -z "$subjects" ]; then
	echo "release: no commits since $base — nothing to release"
	exit 0
fi

# Decide the bump. Major wins over minor wins over patch.
major=0 minor=0 patch=0
while IFS= read -r s; do
	[ -z "$s" ] && continue
	if printf '%s' "$s" | grep -Eq '^[a-z]+(\([^)]+\))?!:'; then major=1; fi
	case "$s" in
	feat:* | feat\(*) minor=1 ;;
	fix:* | fix\(* | perf:* | perf\(*) patch=1 ;;
	esac
done <<EOF
$subjects
EOF
printf '%s' "$bodies" | grep -q 'BREAKING CHANGE' && major=1

if [ "$major" = 1 ]; then part=major
elif [ "$minor" = 1 ]; then part=minor
elif [ "$patch" = 1 ]; then part=patch
else
	echo "release: only non-releasing commits since $base (docs/chore/ci/…) — nothing to release"
	exit 0
fi

IFS=. read -r MA MI PA <<EOF
$cur
EOF
case "$part" in
major) MA=$((MA + 1)); MI=0; PA=0 ;;
minor) MI=$((MI + 1)); PA=0 ;;
patch) PA=$((PA + 1)) ;;
esac
new="$MA.$MI.$PA"

# Build the changelog section: group commits by type under Keep-a-Changelog
# headings, stripping the "type(scope): " prefix from each subject.
notes="$(mktemp)"
{
	printf '## [%s] - %s\n\n' "$new" "$(date +%Y-%m-%d)"
	for grp in "feat:Features" "fix:Bug Fixes" "perf:Performance" "refactor:Refactoring" "docs:Documentation"; do
		type="${grp%%:*}"
		title="${grp#*:}"
		items="$(printf '%s\n' "$subjects" | grep -E "^${type}(\(|!|:)" | sed -E "s/^${type}(\([^)]*\))?!?: //" || true)"
		if [ -n "$items" ]; then
			printf '### %s\n\n' "$title"
			printf '%s\n' "$items" | sed 's/^/- /'
			printf '\n'
		fi
	done
} >"$notes"

echo "release: $cur → $new ($part)"
echo "----- changelog -----"
cat "$notes"
echo "---------------------"

if [ "$dry" = 1 ]; then
	rm -f "$notes"
	echo "[dry-run] no changes written"
	exit 0
fi

# Insert the new section above the first existing "## [" release heading.
awk -v nf="$notes" '
	BEGIN { while ((getline l < nf) > 0) buf = buf l "\n" }
	!done && /^## \[/ { printf "%s", buf; done=1 }
	{ print }
	END { if (!done) printf "%s", buf }
' CHANGELOG.md >CHANGELOG.md.tmp && mv CHANGELOG.md.tmp CHANGELOG.md
rm -f "$notes"

printf '%s\n' "$new" >version.txt

git add version.txt CHANGELOG.md
git commit -q -m "chore(release): v$new"
git tag -a "v$new" -m "v$new"

echo "release: committed + tagged v$new"
echo "push with:  git push origin HEAD --follow-tags"
