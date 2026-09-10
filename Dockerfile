# syntax=docker/dockerfile:1

FROM golang:1.26.4-bookworm AS build

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH:-$(go env GOARCH)} go build -trimpath -ldflags='-s -w' \
	-o /out/vault-replica-controller ./cmd/vault-replica-controller

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/vault-replica-controller /vault-replica-controller
USER 65532:65532
ENTRYPOINT ["/vault-replica-controller"]
