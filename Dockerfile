FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/tollgate ./cmd/tollgate

# distroless/static ships CA certs and a nonroot user; the binary is CGO-free.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tollgate /tollgate
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/tollgate"]
CMD ["--config", "/etc/tollgate/config.yaml"]
