# Многостадийная сборка (бонусный Фактор 14).
#
# Стадия 1 (builder) содержит компилятор Go, исходники, кэш модулей — ~900 МБ.
# Стадия 2 содержит один статический бинарник. В финальный образ попадает
# только вторая стадия.
#
# В терминах 12 Factor: Фактор 5 требует строгого разделения стадий, и образ —
# это артефакт стадии build. В нём не должно быть ничего, что нужно только
# для сборки.

# ---------- build ----------
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Сначала ТОЛЬКО go.mod/go.sum и загрузка зависимостей.
# Это не стилистика, а работа с кэшем слоёв: слой инвалидируется при
# изменении своих входных файлов. Правка main.go не должна приводить
# к повторному скачиванию всех модулей — а при `COPY . .` одной командой
# будет именно так.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 — статическая линковка. Без неё Go тянет системный резолвер
# через cgo, и бинарник, собранный на glibc, падает на musl с «no such file
# or directory» — сообщение, совершенно не наводящее на мысль о линковке.
# Для distroless/static статическая линковка обязательна: там нет libc вообще.
#
# -trimpath убирает пути сборочной машины из бинарника (иначе внутри окажется
# /home/ivan/... — и утечка, и помеха воспроизводимости сборки).
#
# -ldflags "-s -w" выбрасывает таблицу символов и DWARF: минус ~25% размера.
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/scheduler .

# ---------- runtime ----------
#
# ВЫБОР БАЗОВОГО ОБРАЗА — измерено, а не взято на веру.
# Один и тот же бинарник на трёх базах:
#
#   база                          размер   пакетов ОС   sh    tzdata  CA certs
#   alpine:3.22 + ca-certs+tzdata  38.6 МБ     18       есть    да      да
#   distroless/static-debian12     28.7 МБ      4       НЕТ     да      да
#   scratch                        22.6 МБ      0       НЕТ    НЕТ     НЕТ
#
# scratch отпадает не из-за принципов, а потому что ЛОМАЕТ сервисы:
#   * scheduler падает на time.LoadLocation("Europe/Moscow") — нет tzdata;
#   * telegram-api не может открыть HTTPS к api.telegram.org — нет корневых
#     сертификатов (x509: certificate signed by unknown authority).
# Проверено запуском, а не предположено.
#
# distroless — компромисс: минус 26% размера и минус 78% пакетов
# относительно alpine, при этом tzdata и CA-сертификаты на месте.
#
# ЧТО МЫ ТЕРЯЕМ, И ЭТО НАДО ЗНАТЬ ЗАРАНЕЕ: в образе нет оболочки.
# `kubectl exec -it pod -- sh` работать не будет. Прямой запуск бинарника
# работает (`kubectl exec deploy/users -- users admin stats`), а для отладки
# есть эфемерные контейнеры: `kubectl debug -it pod --image=busybox
# --target=users`. Отсутствие sh — это ещё и защита: атакующий,
# получивший выполнение кода, не найдёт ни оболочки, ни curl, ни wget.
FROM gcr.io/distroless/static-debian12:nonroot

# 65532 — пользователь `nonroot`, который РЕАЛЬНО ЕСТЬ в /etc/passwd
# этого образа. Числом, а не именем: securityContext.runAsNonRoot
# в Kubernetes умеет проверить «не root» только по числовому UID.
USER 65532:65532

COPY --from=builder /out/scheduler /usr/local/bin/scheduler

# EXPOSE — документация, а не настройка: он ничего не открывает.
# Реальный порт задаётся переменной SERVER_PORT (Фактор 3).
EXPOSE 8080

# Форма exec (JSON-массив), а не shell. В shell-форме PID 1 стал бы /bin/sh,
# и SIGTERM от Kubernetes пришёл бы ему, а не приложению — graceful shutdown
# (Фактор 9) не сработал бы никогда. В distroless шелла нет вовсе,
# так что другая форма тут просто не запустится.
ENTRYPOINT ["/usr/local/bin/scheduler"]
