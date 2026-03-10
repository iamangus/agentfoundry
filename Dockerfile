FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /agentfile ./cmd/agentfile/

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /agentfile /usr/local/bin/agentfile
COPY definitions/ /data/definitions/

WORKDIR /data

EXPOSE 3000

ENTRYPOINT ["agentfile"]
