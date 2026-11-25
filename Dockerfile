FROM node:22-bookworm-slim AS web
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal/ ./internal/
COPY spec/ ./spec/
COPY --from=web /src/frontend/dist/ ./frontend/dist/
RUN CGO_ENABLED=0 go build -trimpath -o /bambastic .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg poppler-utils tzdata && rm -rf /var/lib/apt/lists/* && useradd --system --uid 10001 --create-home bambastic && install -d -o bambastic -g bambastic -m 700 /data
COPY --from=build /bambastic /usr/local/bin/bambastic
USER bambastic
ENV BAMBASTIC_DATA_DIR=/data BAMBASTIC_ADDR=0.0.0.0:8080
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["bambastic"]
CMD ["serve"]
