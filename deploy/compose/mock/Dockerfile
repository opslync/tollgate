FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /out/mock-anthropic .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mock-anthropic /mock-anthropic
EXPOSE 9911
USER nonroot
ENTRYPOINT ["/mock-anthropic"]
