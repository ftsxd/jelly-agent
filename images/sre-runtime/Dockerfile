# Supply CLI versions deliberately; do not upgrade tools during agent execution.
# Production builds should supply BASE_IMAGE with an approved sha256 digest.
ARG BASE_IMAGE=python:3.12-slim-bookworm
FROM ${BASE_IMAGE}
ARG TCCLI_VERSION
ARG KUBECTL_VERSION
ARG TARGETARCH
RUN test -n "${TCCLI_VERSION}" && test -n "${KUBECTL_VERSION}" \
    && apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl jq \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir "tccli==${TCCLI_VERSION}"
RUN case "${TARGETARCH}" in amd64|arm64) ;; *) exit 1 ;; esac \
    && curl -fsSLo /tmp/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
    && curl -fsSLo /tmp/kubectl.sha256 "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256" \
    && cd /tmp && printf '%s  kubectl\n' "$(cat kubectl.sha256)" | sha256sum --check \
    && install -m 0755 kubectl /usr/local/bin/kubectl \
    && rm /tmp/kubectl /tmp/kubectl.sha256
WORKDIR /work
# No application credentials, kubeconfig, or server state are included.
CMD ["tccli", "help"]
