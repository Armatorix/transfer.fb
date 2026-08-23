# Default to Go 1.24
ARG GO_VERSION=1.24
FROM golang:${GO_VERSION}-alpine as build

# Necessary to run 'go get' and to compile the linked binary
RUN apk add git musl-dev

WORKDIR /go/src/github.com/dutchcoders/transfer.sh

COPY go.mod go.sum ./

RUN go mod download

COPY . .

# build & install server
RUN CGO_ENABLED=0 go build -tags netgo -ldflags "-X github.com/dutchcoders/transfer.sh/cmd.Version=$(git describe --tags) -a -s -w -extldflags '-static'" -o /go/bin/transfersh

FROM alpine:3.21 AS final
LABEL maintainer="Andrea Spacca <andrea.spacca@gmail.com>"

ARG PUID=5000 \
    PGID=5000 \
    RUNAS

# ca-certificates and mime.types are needed by the server itself, yt-dlp needs
# python and relies on ffmpeg to merge and convert the media it downloads
RUN apk add --no-cache ca-certificates mailcap ffmpeg python3 && \
    apk add --no-cache --virtual .ytdlp-deps py3-pip && \
    python3 -m venv /opt/yt-dlp && \
    /opt/yt-dlp/bin/pip install --no-cache-dir yt-dlp && \
    ln -s /opt/yt-dlp/bin/yt-dlp /usr/local/bin/yt-dlp && \
    apk del .ytdlp-deps && \
    yt-dlp --version

RUN if [ ! -z "$RUNAS" ]; then \
    addgroup -g "${PGID}" "${RUNAS}" && \
    adduser -D -H -u "${PUID}" -G "${RUNAS}" -s /sbin/nologin "${RUNAS}"; fi

COPY --from=build --chown=${RUNAS} /go/bin/transfersh /go/bin/transfersh

USER ${RUNAS}

ENTRYPOINT ["/go/bin/transfersh", "--listener", ":8080"]

EXPOSE 8080
