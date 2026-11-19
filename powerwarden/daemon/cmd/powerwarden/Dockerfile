# powerwarden — build from the repository root:
#   docker build -f powerwarden/daemon/cmd/powerwarden/Dockerfile .
# (`just image` in powerwarden/daemon does this and stages IMAGE_CA_CERT.)
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN cd powerwarden/daemon && CGO_ENABLED=0 go build -o /out/powerwarden ./cmd/powerwarden

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY powerwarden/daemon/cmd/powerwarden/extra-ca/ /usr/local/share/ca-certificates/extra/
RUN update-ca-certificates
COPY --from=build /out/powerwarden /usr/local/bin/powerwarden
COPY powerwarden/daemon/config.yaml.example /etc/powerwarden/config.yaml.example
VOLUME ["/var/lib/powerwarden"]
EXPOSE 8091
ENTRYPOINT ["/usr/local/bin/powerwarden"]
CMD ["serve", "--config", "/etc/powerwarden/config.yaml"]
