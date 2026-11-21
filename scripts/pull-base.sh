#!/bin/sh
# Pull IMAGE, a base pinned by digest, for PLATFORM into the Docker daemon's
# store, retrying with a backoff, before a docker buildx build for PLATFORM
# starts FROM it. Buildx's docker
# driver then takes a digest it holds from the store and never asks the
# registry, so a registry that fails a request now and then (public ECR,
# vm#203) is retried here instead of failing the build. Another builder
# (docker-container) keeps a store of its own and pulls for itself, so the
# pull does not reach it. PIN says how to move the pin, for the message when
# every attempt fails.
#
#   scripts/pull-base.sh IMAGE PLATFORM PIN
#
# PULL_BASE_ATTEMPTS (5) and PULL_BASE_DELAY (5 seconds, doubled after each
# failure) set the retries.
set -eu
[ $# -eq 3 ] || { echo "usage: $0 IMAGE PLATFORM PIN" >&2; exit 2; }
image=$1
platform=$2
pin=$3
attempts=${PULL_BASE_ATTEMPTS:-5}
delay=${PULL_BASE_DELAY:-5}

case $image in
*@sha256:*) ;;
*) echo "pull-base: $image is not pinned by digest: $pin" >&2; exit 2 ;;
esac

if ! docker version >/dev/null; then
	echo "pull-base: the Docker daemon did not answer (its error is above), so $image was not pulled" >&2
	exit 1
fi

# inspect --platform answers 0 for a platform the store does not hold, with
# no OS (Docker 29.9.3), so only an OS counts as held.
if [ -n "$(docker image inspect --platform "$platform" --format '{{.Os}}' "$image" 2>/dev/null)" ]; then
	exit 0
fi

n=1
while :; do
	if docker pull --quiet --platform "$platform" "$image" >/dev/null; then
		echo "pull-base: pulled $image for $platform"
		exit 0
	fi
	[ "$n" -lt "$attempts" ] || break
	echo "pull-base: pulling $image for $platform failed (attempt $n of $attempts; docker's error is above); retrying in ${delay}s" >&2
	sleep "$delay"
	n=$((n + 1))
	delay=$((delay * 2))
done

# Say which it was where the registry can tell: what it gives for the pin's
# tag now. Its error alone does not: public ECR answers a digest it does not
# have with 401 Unauthorized, as it would a bad credential.
ref=${image%@*}
digest=${image#*@}
now=
case ${ref##*/} in
*:*) now=$(docker buildx imagetools inspect --format '{{json .Manifest.Digest}}' "$ref" 2>/dev/null | tr -d '"') || now= ;;
esac
if [ "$now" = "$digest" ]; then
	verdict="The registry still gives $ref as that digest, so the pin holds: docker's errors above say what failed (a registry or network failing for a while: run the build again)."
elif [ -n "$now" ]; then
	verdict="The registry now gives $ref as $now, not that digest, which was likely removed upstream: $pin."
else
	verdict="If the registry answers but not for that digest (public ECR says 401 Unauthorized for a digest it does not have), it was removed upstream: $pin. Otherwise the registry or the network failed: run the build again."
fi
cat >&2 <<EOF
pull-base: pulling $image for $platform failed $attempts times, before the
pull-base: build started: the pull failed, not the build. Docker's errors are above.
pull-base: Digest: $digest
pull-base: $verdict
EOF
exit 1
