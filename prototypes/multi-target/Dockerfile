# Prototype image: one service, many databases.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pg-backup .

FROM alpine:3.23
# One client per supported server major; the app picks the matching one.
RUN apk add --no-cache postgresql16-client postgresql17-client postgresql18-client \
      rclone ca-certificates tzdata
COPY --from=build /pg-backup /usr/local/bin/pg-backup
ENV DATA_DIR=/data
VOLUME /data
ENTRYPOINT ["/usr/local/bin/pg-backup"]
