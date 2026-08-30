# ─────────────────────────────────────────────────────────────────────────────
# EN: Multi-stage build. Stage 1 has the Go toolchain (~800 MB of compiler,
#     source and module cache). Stage 2 keeps ONLY the resulting binary.
#     Two separate wins, worth naming separately:
#       · size  — a small image pulls faster, so a node that scales up joins the
#                 pool sooner. Image size is a latency number in disguise.
#       · attack surface — the compiler, git, package manager and shell simply do
#                 not exist in the shipped image, so they cannot be used against
#                 you. This is least privilege applied to a filesystem.
# TR: Çok aşamalı build. 1. aşamada Go araç zinciri var (~800 MB derleyici, kaynak
#     ve modül önbelleği). 2. aşama YALNIZCA çıkan binary'yi taşıyor.
#     Ayrı ayrı isimlendirilmeyi hak eden iki kazanç:
#       · boyut — küçük imaj daha hızlı çekilir, yani ölçeklenen bir node havuza
#                 daha erken katılır. İmaj boyutu, kılık değiştirmiş bir gecikme sayısı.
#       · saldırı yüzeyi — derleyici, git, paket yöneticisi ve shell gönderilen
#                 imajda hiç yok, dolayısıyla sana karşı kullanılamaz. En az yetki
#                 ilkesinin dosya sistemine uygulanmış hâli.
# [Topic · Konu: En az yetki, soğuk başlangıç]
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build

WORKDIR /src

# EN: Copy the module files first and download dependencies as their own layer.
#     Source changes far more often than go.mod does, so this layer stays cached
#     across almost every rebuild. Docker layer caching is cache-aside with a
#     content hash as the key — the same idea as internal/cache, one level down.
# TR: Önce modül dosyalarını kopyalayıp bağımlılıkları kendi katmanında indir.
#     Kaynak, go.mod'dan çok daha sık değişir; bu yüzden bu katman neredeyse her
#     yeniden build'de cache'te kalır. Docker katman cache'i, anahtarı içerik
#     hash'i olan bir cache-aside — internal/cache ile aynı fikir, bir kat aşağıda.
COPY go.mod ./
RUN go mod download

COPY . .

# EN: CGO_ENABLED=0 produces a fully static binary with no libc dependency, which
#     is what lets the runtime stage be `static` rather than a full distro.
#     -trimpath strips local filesystem paths out of the binary (they are a small
#     information leak and they break reproducible builds).
#     -ldflags "-s -w" drops the symbol table and DWARF data: a smaller binary, and
#     one that is marginally less convenient to reverse engineer.
# TR: CGO_ENABLED=0, libc bağımlılığı olmayan tamamen statik bir binary üretir;
#     çalışma aşamasının tam bir dağıtım yerine `static` olabilmesini sağlayan şey bu.
#     -trimpath, yerel dosya yollarını binary'den siler (küçük bir bilgi sızıntısı
#     ve tekrarlanabilir build'i bozarlar).
#     -ldflags "-s -w" sembol tablosunu ve DWARF verisini atar: daha küçük binary ve
#     tersine mühendisliği bir tık daha zahmetli olan bir binary.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/linkly ./cmd/linkly

# EN: Run the test suite inside the build so a broken commit cannot produce an
#     image at all. Cheap gate, and it fails where it is easy to read.
# TR: Test paketini build'in içinde çalıştır ki bozuk bir commit imaj üretemesin.
#     Ucuz bir kapı ve okunması kolay bir yerde patlıyor.
RUN go vet ./... && go test ./...

# ─────────────────────────────────────────────────────────────────────────────
# Runtime stage
# ─────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# EN: `nonroot` runs as UID 65532. Running as root inside a container is the
#     default almost everywhere and it is almost always wrong: if anything in the
#     process is compromised, root inside the container is a much better starting
#     position than an unprivileged user. Least privilege, one line.
# TR: `nonroot` UID 65532 ile çalışır. Konteyner içinde root olarak çalışmak
#     neredeyse her yerde varsayılan ve neredeyse her zaman yanlış: süreçte bir şey
#     ele geçirilirse, konteyner içinde root olmak yetkisiz bir kullanıcı olmaktan
#     çok daha iyi bir başlangıç noktasıdır. En az yetki, tek satır.
USER nonroot:nonroot

COPY --from=build /out/linkly /linkly

EXPOSE 8080

# EN: The healthcheck calls the binary's own `healthcheck` subcommand, because
#     there is no shell or curl in this image to call anything else. It probes
#     /healthz — liveness only. Deliberately NOT a dependency check: if a shared
#     dependency blips and every replica reports unhealthy at once, the orchestrator
#     restarts the whole fleet and you have added a cold-cache stampede on top of
#     an already sick dependency.
# TR: Healthcheck, binary'nin kendi `healthcheck` alt komutunu çağırıyor; çünkü bu
#     imajda başka bir şeyi çağıracak ne shell ne curl var. /healthz'i yokluyor —
#     yalnızca liveness. Bilinçli olarak bağımlılık kontrolü DEĞİL: paylaşılan bir
#     bağımlılık bir an takılır ve bütün replikalar aynı anda sağlıksız derse,
#     orkestratör tüm filoyu yeniden başlatır ve zaten hasta olan bağımlılığın
#     üstüne bir de soğuk cache dalgası eklemiş olursun.
# [Topic · Konu: Liveness ≠ readiness]
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/linkly", "healthcheck"]

# EN: ENTRYPOINT in exec form (no shell). This matters for shutdown: with the shell
#     form, PID 1 is /bin/sh and it does NOT forward SIGTERM to your process — the
#     graceful drain in main.go would simply never run, and Docker would SIGKILL
#     after the grace period. Every carefully ordered Close() in this codebase is
#     worth exactly nothing if the signal never arrives.
# TR: ENTRYPOINT exec biçiminde (shell yok). Bu, kapatma için önemli: shell
#     biçiminde PID 1 /bin/sh olur ve SIGTERM'i senin sürecine İLETMEZ —
#     main.go'daki nazik drain hiç çalışmaz, Docker da bekleme süresi sonunda
#     SIGKILL gönderir. Bu kod tabanındaki özenle sıralanmış her Close(), sinyal
#     hiç gelmiyorsa tam olarak hiçbir işe yaramaz.
# [Topic · Konu: Graceful shutdown]
ENTRYPOINT ["/linkly"]
