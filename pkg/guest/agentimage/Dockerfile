# disco-vm's agent as an image (discobox ADR 26-11-11-963 §10, ADR
# 26-11-13-318 §3): the disco-vm of one module version for one platform at
# /disco-vm, and nothing else, one per platform under one index: static
# linux/amd64 and linux/arm64, and darwin/arm64. The context is
# build/agent-image, which holds a binary per platform (go tool task
# push:agent); agent.Pull pulls it by digest.
FROM scratch
ARG TARGETOS
ARG TARGETARCH
LABEL org.opencontainers.image.source=https://github.com/discobox-ai/vm
LABEL org.opencontainers.image.description="disco-vm's guest agent"
COPY --chmod=0755 ${TARGETOS}-${TARGETARCH}/disco-vm /disco-vm
ENTRYPOINT ["/disco-vm"]
