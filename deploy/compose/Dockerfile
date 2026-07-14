# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/walletd ./cmd/walletd

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/walletd /usr/local/bin/walletd
ENTRYPOINT ["/usr/local/bin/walletd"]
