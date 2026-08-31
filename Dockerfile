# dop-core — UMA imagem, QUATRO modos (ADR-0016).
# O modo é o primeiro argumento: serve | worker | sched | launcher.
# Um artefato, um pipeline: o que roda em produção é o mesmo binário do local.

# ── build ────────────────────────────────────────────────────────────────────
FROM golang:1.27-alpine AS build

WORKDIR /src

# As dependências mudam muito menos que o código: baixar antes de copiar o
# fonte preserva a camada de módulos entre builds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO desligado: binário estático, roda em imagem sem libc do sistema e não
# depende do resolvedor em C (o resolvedor puro-Go funciona com o DNS do cluster).
# -trimpath tira o caminho da máquina de build do binário — build reprodutível.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w" \
      -o /out/dop-core ./cmd/dop-core

# ── runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.22

ARG VERSION=dev
LABEL org.opencontainers.image.title="dop-core" \
      org.opencontainers.image.version="${VERSION}"

# ca-certificates: TLS com a API do Kubernetes e com o Firebase/GCS.
# tzdata: o scheduler compara horários; sem base de fusos tudo vira UTC calado.
RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/dop-core /app/dop-core

# Usuário arbitrário não-root (requisito OKD/OpenShift — spec do substrato §2).
# O OKD atribui um UID que NÃO existe em /etc/passwd e coloca o processo no
# grupo 0; por isso a permissão vive no grupo root, nunca num usuário nomeado.
ENV HOME=/app
RUN chgrp -R 0 /app && chmod -R g=u /app

WORKDIR /app
USER 1001
EXPOSE 9090 9091

ENTRYPOINT ["/app/dop-core"]
# Sem modo declarado o container serve gRPC; o Deployment sobrescreve com args.
CMD ["serve"]
