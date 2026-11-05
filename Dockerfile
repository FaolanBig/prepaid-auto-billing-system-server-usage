FROM golang:1.24-alpine AS build

RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/power-billing ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/power-billing /power-billing
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
EXPOSE 8080
ENTRYPOINT ["/power-billing"]
