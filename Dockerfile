# syntax=docker/dockerfile:1

FROM golang:1.26.3-bookworm AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
	-o /out/vault-replica-controller ./cmd/vault-replica-controller

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/vault-replica-controller /vault-replica-controller
USER nonroot:nonroot
ENTRYPOINT ["/vault-replica-controller"]