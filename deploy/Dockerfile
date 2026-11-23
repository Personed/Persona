ARG GO_IMAGE=golang:1.27-bookworm
ARG NODE_IMAGE=node:24-bookworm-slim
ARG DOCKER_CLI_IMAGE=docker:29-cli
FROM ${DOCKER_CLI_IMAGE} AS docker
FROM ${NODE_IMAGE} AS web
WORKDIR /app
RUN npm install --global pnpm@10.20.0
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY patches ./patches
COPY apps ./apps
COPY packages ./packages
RUN pnpm install --frozen-lockfile --ignore-scripts && pnpm build

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY apps/web ./apps/web
COPY --from=web /app/apps/web/dist ./apps/web/dist
RUN CGO_ENABLED=0 go build -tags embedweb -ldflags "-X github.com/Personed/Persona/internal/cli.Version=0.1.0" -o /out/persona ./cmd/persona && install -d -m 700 /data

FROM gcr.io/distroless/static-debian12
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=build /out/persona /usr/local/bin/persona
COPY --from=build --chown=1000:1000 /data /data
COPY contracts/api /app/contracts/api
COPY deploy/release.json /app/deploy/release.json
COPY deploy/NOTICE /usr/share/doc/persona/NOTICE
COPY deploy/licenses/ /usr/share/doc/persona/licenses/
COPY persona.config.json /app/persona.config.json
WORKDIR /app
ENV PERSONA_HOST=0.0.0.0 PERSONA_DATA_DIR=/data PERSONA_DEPLOYMENT=container PERSONA_PUBLIC_URL=http://localhost:4310
USER 1000:1000
EXPOSE 4310
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s CMD ["persona", "health", "http://127.0.0.1:4310/health"]
ENTRYPOINT ["persona"]
CMD ["serve"]
