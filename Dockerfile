FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -trimpath -ldflags="-s -w" -o /minibrainz .

FROM debian:bookworm-slim
# lbzip2 decompresses the weekly dump on every core.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates lbzip2 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /minibrainz /usr/local/bin/minibrainz
ENV MINIBRAINZ_DATA=/data
VOLUME /data
EXPOSE 8080
# An absolute path: some container hosts replace the image environment, PATH included.
CMD ["/usr/local/bin/minibrainz", "serve"]
