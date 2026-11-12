# disco-vm's linux agent as an image (discobox ADR 26-11-11-963 §10): the
# static linux disco-vm of one module version at /disco-vm, and nothing else,
# one per platform under one index. The context is build/linux-agent-image,
# which holds a binary per architecture (go tool task push:linux-agent);
# linuximage.PullAgent pulls it by digest.
FROM scratch
ARG TARGETARCH
LABEL org.opencontainers.image.source=https://github.com/discobox-ai/vm
LABEL org.opencontainers.image.description="disco-vm's static linux agent"
COPY --chmod=0755 linux-${TARGETARCH}/disco-vm /disco-vm
ENTRYPOINT ["/disco-vm"]
