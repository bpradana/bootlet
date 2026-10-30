# Build from the repository root: docker build -f apps/relay/Dockerfile -t bootlet-relay .
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY packages/shared/go.mod packages/shared/
COPY packages/shared/wisp packages/shared/wisp
COPY apps/relay apps/relay
WORKDIR /src/apps/relay
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /relay .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /relay /relay
EXPOSE 8089
ENTRYPOINT ["/relay", "-addr", ":8089"]
