FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /agentfoundry ./cmd/agentfoundry/

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /agentfoundry /usr/local/bin/agentfoundry
COPY definitions/ /data/definitions/

WORKDIR /data

EXPOSE 3000

ENTRYPOINT ["agentfoundry"]
