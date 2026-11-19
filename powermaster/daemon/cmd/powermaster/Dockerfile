# Build context is the repo root (the module uses go.work siblings):
#   just --justfile powermaster/daemon/justfile -d powermaster/daemon image
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN cd powermaster/daemon && CGO_ENABLED=0 go build -o /out/powermaster ./cmd/powermaster

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates iputils-ping openssh-client sshpass curl \
 && rm -rf /var/lib/apt/lists/*
COPY powermaster/daemon/cmd/powermaster/extra-ca/ /usr/local/share/ca-certificates/extra/
RUN update-ca-certificates
COPY --from=build /out/powermaster /usr/local/bin/powermaster
COPY powermaster/daemon/config.yaml.example /etc/powermaster/config.yaml.example
VOLUME ["/var/lib/powermaster"]
EXPOSE 8092
ENTRYPOINT ["/usr/local/bin/powermaster"]
CMD ["serve", "--config", "/etc/powermaster/config.yaml"]
