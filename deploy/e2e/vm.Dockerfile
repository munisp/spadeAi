# Disposable "managed VM" for overlay e2e tests: its own Docker daemon, network namespace and kernel
# WireGuard/TUN access (run with --privileged). Build context must contain spade-agent (static musl),
# nebula, nebula-cert and the agent CA (ca.crt).
FROM docker:27-dind
RUN apk add --no-cache wireguard-tools iproute2 iputils curl restic
COPY spade-agent nebula nebula-cert /usr/local/bin/
COPY ca.crt /etc/spade/ca.pem
