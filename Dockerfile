FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /nimlet ./cmd/nimlet

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata bash

COPY --from=builder /nimlet /usr/local/bin/nimlet

EXPOSE 8080
ENTRYPOINT ["nimlet"]
CMD ["server"]
