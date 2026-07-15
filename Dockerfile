FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/jelly ./cmd/cli

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/jelly /usr/local/bin/jelly
USER nonroot:nonroot
ENV HOME=/data
EXPOSE 6185
ENTRYPOINT ["/usr/local/bin/jelly"]
CMD ["serve", "--addr", "0.0.0.0:6185"]
