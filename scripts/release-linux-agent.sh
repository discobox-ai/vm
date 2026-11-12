#!/bin/sh
# Publish the linux agent of every commit a push to main brought, BEFORE..HEAD
# (BEFORE the branch's head before the push), oldest first, or of HEAD alone
# when there is no BEFORE this clone has (a tag, a new branch, a force-push):
# each commit is a module version an embedder may require. Each is built from
# a worktree of its own commit, with this checkout's Taskfile. A version
# already in REPOSITORY is not pushed again, so the digest its tag names never
# moves, and is checked as a new one is; two runs of one commit (a branch and
# a tag pushed together) take turns (linux-agent.yml).
set -eu
before=${1:-}
repository=${REPOSITORY:-ghcr.io/discobox-ai/disco-vm-agent}
if [ -n "$before" ] && git cat-file -e "$before^{commit}" 2>/dev/null; then
	revs=$(git rev-list --reverse "$before..HEAD")
else
	revs=$(git rev-parse HEAD)
fi
src=build/linux-agent-src
for rev in $revs; do
	version=$(scripts/module-version.sh "$rev")
	# Pushed only when the registry says the tag is not there: any other
	# failure to ask stops the release rather than push over a published tag.
	if out=$(docker buildx imagetools inspect "$repository:$version" --format '{{.Manifest.Digest}}' 2>&1); then
		echo "$repository:$version is published already"
		go tool task check:linux-agent IMAGE="$repository@$out" MODULE_VERSION="$version"
		continue
	fi
	case $out in
	*"not found"*) ;;
	*)
		echo "$repository:$version: $out" >&2
		exit 1
		;;
	esac
	git worktree remove --force "$src" 2>/dev/null || rm -rf "$src"
	git worktree add --detach "$src" "$rev"
	go tool task push:linux-agent REPOSITORY="$repository" MODULE_VERSION="$version" SRC="$src"
	git worktree remove --force "$src"
done
