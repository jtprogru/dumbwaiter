# dumbwaiter

Минимальный reverse proxy, который раздаёт статические сайты из S3-совместимого хранилища. Рассчитан на Kubernetes: один бинарник на stdlib плюс четыре зависимости (SigV4-подпись из aws-sdk-go-v2, quic-go для HTTP/3, yaml, VictoriaMetrics/metrics для метрик), distroless-образ, конфиг в YAML. Так же запускается на отдельном сервере в Docker Compose или как systemd-сервис.

## Что умеет

- Маршрутизация по `Host`: несколько доменов на один сайт, несколько сайтов на один инстанс, у каждого свой бакет и префикс.
- Только `GET` и `HEAD`. На остальные методы отвечает `405` с заголовком `Allow: GET, HEAD`.
- `/` и `/docs/` отдают `index.html`, `/docs` редиректит на `/docs/`, как nginx и S3 website hosting.
- Своя страница 404 (`not_found`) и SPA-режим: `/users/42` получает корневой `index.html`, а отсутствующий `/app.js` честно получает 404.
- `Range`, `If-None-Match`, `If-Modified-Since` уходят в S3 как есть, поэтому `206` и `304` работают без кэша в прокси.
- `Content-Encoding` объекта не трогается: заранее сжатые `*.gz` с правильными метаданными уходят клиенту сжатыми.
- Если объект залит как `binary/octet-stream`, тип угадывается по расширению.
- Заголовки ответа из конфига, например HSTS или `Cache-Control`. Они перекрывают метаданные объекта.
- Доступ к S3 с подписью SigV4 по статическим ключам или анонимно к публичному бакету, адресация path-style или virtual-hosted.
- Опционально: TLS с выбором сертификата по SNI, HTTP/2 и HTTP/3. Сертификаты перечитываются с диска, обновлённый cert-manager'ом Secret подхватывается без рестарта.
- `/healthz`, `/readyz` и метрики Prometheus на отдельном admin-порту, graceful shutdown под SIGTERM.

## Где терминировать TLS

Есть два режима, выбор зависит от того, что стоит перед прокси.

TLS на Ingress или Gateway. Прокси слушает plain HTTP на `:8080`, сертификаты и HTTP/3 живут на edge. Это самый простой вариант, и если в кластере уже есть Gateway API или Ingress-контроллер, лучше выбрать его. За edge прокси всё равно получит HTTP/1.1, так что HTTP/3 в нём самом здесь бесполезен.

TLS в прокси. Блок `server.https` в конфиге включает TLS на `:8443`, HTTP/2 и, по флагу `http3: true`, HTTP/3 на том же порту по UDP. Прокси публикуется через Service типа `LoadBalancer` с TCP и UDP на 443. Ответы по TCP получают `Alt-Svc`, по нему браузер переходит на HTTP/3.

## Конфиг

Полный справочник с комментариями лежит в [`config.example.yaml`](config.example.yaml). Минимальный конфиг:

```yaml
s3:
  endpoint: https://storage.yandexcloud.net
  region: ru-central1
  access_key_id: ${S3_ACCESS_KEY_ID}
  secret_access_key: ${S3_SECRET_ACCESS_KEY}

sites:
  - hosts: [example.com, www.example.com]
    bucket: example-com
    not_found: 404.html
```

`${VAR}` подставляется из окружения, только в значениях и никогда в комментариях. Незаданная переменная останавливает запуск: пустой секретный ключ лучше поймать на старте, чем разбирать потом 403 от S3. Неизвестные поля тоже считаются ошибкой, опечатка в `buckt` не пройдёт молча.

Проверить конфиг без запуска:

```bash
dumbwaiter -check -config config.yaml
```

Ключевые моменты:

- `path_style: true` нужен для MinIO, Ceph и большинства self-hosted хранилищ. Для AWS оставь `false`.
- Без `access_key_id` и `secret_access_key` запросы уходят анонимно.
- У сайта может быть свой блок `s3`. Он заменяет верхний целиком, ничего не наследуется.
- Если у ключа нет права `s3:ListBucket`, S3 отвечает на отсутствующий объект `403 AccessDenied` вместо `404`. Прокси считает это за 404. Другие 403, например `SignatureDoesNotMatch`, превращаются в `502` и пишутся в лог с кодом ошибки.

## Kubernetes

Есть Helm-чарт и kustomize-манифесты, режимы у них одинаковые.

### Helm

Чарт лежит в `deploy/helm/dumbwaiter`. Режим выбирается одним флагом: `httpRoute.enabled` для Gateway API, `ingress.enabled` для Ingress, `tls.enabled` для TLS и HTTP/3 в самом прокси. Конфиг прокси задаётся целиком в `config` в том же формате, что `config.example.yaml`. Адреса слушателей и блок `server.https` чарт собирает сам из `containerPorts` и `tls.*`, поэтому порты в конфиге, pod'е и Service не разъедутся. Хосты для `HTTPRoute`, `Ingress` и `Certificate` по умолчанию берутся из `config.sites`.

```bash
kubectl -n web create secret generic dumbwaiter-s3 \
  --from-literal=S3_ACCESS_KEY_ID=... \
  --from-literal=S3_SECRET_ACCESS_KEY=...

helm upgrade --install dumbwaiter deploy/helm/dumbwaiter -n web -f my-values.yaml
```

`my-values.yaml` для TLS на Gateway:

```yaml
config:
  s3:
    endpoint: https://storage.yandexcloud.net
    region: ru-central1
    access_key_id: ${S3_ACCESS_KEY_ID}
    secret_access_key: ${S3_SECRET_ACCESS_KEY}
  sites:
    - hosts: [example.com, www.example.com]
      bucket: example-com
      not_found: 404.html

s3:
  existingSecret: dumbwaiter-s3

httpRoute:
  enabled: true
  parentRefs:
    - name: public
      namespace: gateway-system
```

Для TLS в прокси хватит `tls.enabled: true` и списка `tls.certificates`. Чарт смонтирует Secret'ы в `/etc/tls/<name>`, создаст `Certificate` для cert-manager и публичный Service `dumbwaiter-public` с 80/TCP, 443/TCP и 443/UDP. Helm 4 применяет манифесты через server-side apply и сохраняет обе записи на 443. Helm 3 строит client-side patch, сливает порты по номеру и на апгрейде может потерять одну из них, поэтому с ним включай `tls.service.separateUDP: true`, и UDP уедет в отдельный Service. Тот же флаг выручает, если балансировщик не умеет смешивать TCP и UDP.

Чарт откажется рендериться, если `server.https` задан руками в `config`, или если `tls.redirectToHTTPS` включён вместе с Ingress или Gateway: edge шлёт в прокси plain HTTP, получает 301 на https, и так по кругу. Примеры values для всех режимов лежат в `deploy/helm/dumbwaiter/ci`, их проверяет `make helm-lint`.

### kustomize

Манифесты лежат в `deploy/kubernetes` в виде kustomize base и трёх overlay:

- `overlays/gateway` добавляет `HTTPRoute` для Gateway API;
- `overlays/ingress` добавляет классический `Ingress` с cert-manager;
- `overlays/tls` включает TLS и HTTP/3 в прокси: `LoadBalancer` на 80/TCP, 443/TCP и 443/UDP, плюс `Certificate` для cert-manager.

Перед применением поправь домены, бакет и endpoint в `config.yaml` нужного overlay, а также имя Gateway, IngressClass или ClusterIssuer.

Ключи к S3 кладутся в Secret, Deployment подхватывает его через `envFrom`:

```bash
kubectl -n web create secret generic dumbwaiter-s3 \
  --from-literal=S3_ACCESS_KEY_ID=... \
  --from-literal=S3_SECRET_ACCESS_KEY=...
```

Режим с TLS на edge:

```bash
kubectl -n web apply -k deploy/kubernetes/overlays/gateway
```

Режим с TLS в прокси, обязательно через server-side apply:

```bash
kubectl -n web apply --server-side -k deploy/kubernetes/overlays/tls
```

Client-side apply сливает порты Service по номеру и теряет одну из двух записей на 443 (TCP или UDP). Server-side apply различает их по паре `port` и `protocol`.

Конфиг монтируется из ConfigMap с хэшем в имени, поэтому любая правка конфига выкатывает новые pod'ы. Горячей перезагрузки конфига нет, и она не нужна.

### Подводные камни режима с TLS

- Смешанные TCP и UDP на одном `LoadBalancer` поддерживаются в Kubernetes с 1.26, но облачный балансировщик тоже должен это уметь. Если не умеет, придётся завести два Service с закреплённым IP. В чарте это `tls.service.separateUDP`, IP закрепляется аннотациями провайдера в `tls.service.annotations`.
- cert-manager нужен с DNS-01. HTTP-01 не сработает: порт 80 балансировщика приходит в прокси, а не в solver cert-manager'а.
- quic-go при старте может предупредить, что не смог увеличить UDP receive buffer. Без привилегий это не лечится изнутри контейнера. Помогает `sysctl net.core.rmem_max=7500000` и `net.core.wmem_max=7500000` на нодах, но и без этого HTTP/3 работает.
- При смене IP клиента (Wi-Fi на LTE) QUIC-соединение может попасть на другой pod и оборваться. Браузер переподключится, но миграция соединений между pod'ами не работает.

## Docker Compose

Для одного сервера в `deploy/compose` есть два варианта.

`compose.yaml` ставит перед прокси Caddy. Caddy сам получает сертификаты Let's Encrypt, редиректит HTTP на HTTPS и отдаёт HTTP/2 и HTTP/3, а прокси получает plain HTTP по внутренней сети compose. Это самый простой путь, кроме A-записи на сервер ничего не нужно:

```bash
cd deploy/compose
cp .env.example .env    # S3-ключи, домены для Caddy, тег образа
$EDITOR config.yaml     # сайты и бакеты
docker compose up -d
docker compose logs -f dumbwaiter
```

Домены перечисляются дважды: в `config.yaml` для маршрутизации и в `SITE_DOMAINS` в `.env` для сертификатов. Caddy передаёт `Host` как есть, так что сайты различаются так же, как без него. В access-логе прокси в поле `remote` будет адрес Caddy, а не клиента.

`compose.tls.yaml` обходится без Caddy: прокси сам слушает 80/TCP, 443/TCP и 443/UDP. Сертификаты кладутся в `deploy/compose/certs/<имя>/tls.crt` и `tls.key`, читать их должен uid или gid 65532, под которым работает образ. Как получать их certbot'ом, описано в разделе про сертификаты ниже.

```bash
echo 'COMPOSE_FILE=compose.tls.yaml' >> .env
$EDITOR config.tls.yaml
docker compose up -d
```

Конфиг читается один раз при старте, после правки нужен `docker compose restart dumbwaiter`. Admin-порт 9090 опубликован только на `127.0.0.1`, в варианте с Caddy там же HTTP-порт 8080. Метрики с хоста забирает vmagent или Prometheus по `http://127.0.0.1:9090/metrics`. С хоста удобно проверять `curl http://127.0.0.1:9090/readyz` и `curl -sI -H 'Host: example.com' http://127.0.0.1:8080/`. Если на сервере уже стоит nginx, можно поднять только прокси (`docker compose up -d dumbwaiter`) и проксировать на `127.0.0.1:8080`. Healthcheck в compose не задан: в distroless-образе нет ни shell, ни curl.

## systemd

Для установки без контейнеров в `deploy/systemd` лежат unit, конфиг, env-файл для ключей и описание системного пользователя. Сервис работает от пользователя `dumbwaiter` в sandbox systemd: файловая система доступна только на чтение, из capabilities оставлен только `CAP_NET_BIND_SERVICE`, чтобы слушать 80 и 443 без root. Конфиг по умолчанию рассчитан на TLS в самом прокси. Если перед ним будет nginx или Caddy на том же хосте, убери `server.https` и `redirect_to_https`, а `server.http.addr` поставь `127.0.0.1:8080`.

Бинарник статический, его можно собрать где угодно:

```bash
GOOS=linux GOARCH=amd64 make build DIST_DIR=dist/linux-amd64
```

Дальше на сервере, из копии репозитория или каталога с этими файлами:

```bash
sudo install -m 0755 dist/linux-amd64/dumbwaiter /usr/local/bin/dumbwaiter
sudo install -m 0644 deploy/systemd/sysusers.conf /etc/sysusers.d/dumbwaiter.conf
sudo systemd-sysusers
sudo install -d -m 0750 -g dumbwaiter /etc/dumbwaiter
sudo install -m 0640 -g dumbwaiter deploy/systemd/config.yaml /etc/dumbwaiter/config.yaml
sudo install -m 0600 deploy/systemd/dumbwaiter.env.example /etc/dumbwaiter/dumbwaiter.env
sudo install -m 0644 deploy/systemd/dumbwaiter.service /etc/systemd/system/dumbwaiter.service
sudo install -m 0644 deploy/host/sysctl.conf /etc/sysctl.d/90-dumbwaiter.conf
sudo sysctl --system

sudoedit /etc/dumbwaiter/dumbwaiter.env   # раскомментировать и заполнить S3-ключи
sudoedit /etc/dumbwaiter/config.yaml      # сайты, бакеты, пути к сертификатам
```

Сертификаты должны появиться до первого запуска, иначе прокси не стартует. Как их получить, описано в следующем разделе. Когда они на месте:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now dumbwaiter
journalctl -u dumbwaiter -f
```

`EnvironmentFile` systemd читает от root до сброса привилегий, поэтому файл с ключами остаётся `0600 root`. В env-файле ключи закомментированы намеренно: пустое значение означало бы анонимный доступ к S3 и поток 403, а незаданная переменная останавливает запуск с понятной ошибкой.

## Сертификаты вне Kubernetes

ACME прокси сам не умеет, сертификаты ему приносит certbot. Хук `deploy/host/certbot-deploy-hook.sh` после каждого выпуска копирует `fullchain.pem` и `privkey.pem` туда, откуда их читает прокси, и выставляет группу. Копировать приходится потому, что certbot держит ключи доступными только root. Прокси перечитывает файлы раз в `reload_interval`, так что продление обходится без рестарта.

```bash
sudo install -m 0755 deploy/host/certbot-deploy-hook.sh /etc/letsencrypt/renewal-hooks/deploy/dumbwaiter
sudo certbot certonly --standalone -d example.com -d www.example.com
```

По умолчанию хук пишет в `/etc/dumbwaiter/tls/<имя>/` с группой `dumbwaiter`, как ждёт конфиг для systemd. Для compose поправь в установленном хуке `TLS_DIR` на абсолютный путь к `deploy/compose/certs` и `TLS_GROUP` на `65532`. Имя каталога совпадает с именем сертификата в certbot, обычно это первый домен.

Первый выпуск через `--standalone` проходит, пока прокси не запущен и порт 80 свободен. При продлении порт 80 уже занят прокси, а раздавать файлы челленджа с диска он не умеет, так что HTTP-01 не пройдёт. Надёжный выход: DNS-01 через плагин certbot для своего DNS-провайдера, тогда прокси в выпуске вообще не участвует, как и с cert-manager в Kubernetes. Выход попроще: останавливать прокси на время продления. Это пара секунд простоя раз в два месяца, certbot вызывает pre- и post-хуки, только когда сертификат действительно продлевается:

```bash
printf '#!/bin/sh\nsystemctl stop dumbwaiter\n' | sudo tee /etc/letsencrypt/renewal-hooks/pre/dumbwaiter
printf '#!/bin/sh\nsystemctl start dumbwaiter\n' | sudo tee /etc/letsencrypt/renewal-hooks/post/dumbwaiter
sudo chmod 0755 /etc/letsencrypt/renewal-hooks/pre/dumbwaiter /etc/letsencrypt/renewal-hooks/post/dumbwaiter
sudo certbot renew --dry-run
```

Для compose в хуках вместо `systemctl` нужен `docker compose -f /путь/к/compose.tls.yaml stop dumbwaiter` и `start dumbwaiter`. Если всё это не хочется настраивать, бери вариант compose с Caddy.

quic-go и в прокси, и в Caddy хочет UDP-буферы около 7 МБ и без них предупреждает при старте. На сервере это лечится `deploy/host/sysctl.conf` в `/etc/sysctl.d/`, изнутри контейнера эти параметры не поднять.

## Метрики

`GET /metrics` на admin-порту отдаёт метрики в текстовом формате Prometheus, его же читают VictoriaMetrics и vmagent. Гистограммы классические, с бакетами `le`, так что `histogram_quantile` работает и в PromQL, и в MetricsQL. Admin-порт не нужно публиковать наружу: в Kubernetes метрики собирают прямо с pod'ов, в Compose и systemd порт слушает только `127.0.0.1`.

| Метрика | Тип | Лейблы | Что показывает |
|---|---|---|---|
| `dumbwaiter_http_requests_total` | counter | `listener`, `proto`, `host`, `method`, `code` | запросы к публичным слушателям |
| `dumbwaiter_http_request_duration_seconds` | histogram | `listener`, `host` | время ответа вместе с передачей тела |
| `dumbwaiter_http_response_bytes_total` | counter | `listener`, `host` | отданные байты тела ответа |
| `dumbwaiter_http_requests_in_flight` | gauge | `listener` | запросы в обработке, включая идущие скачивания |
| `dumbwaiter_s3_requests_total` | counter | `bucket`, `method`, `code` | запросы к S3 |
| `dumbwaiter_s3_request_duration_seconds` | histogram | `bucket`, `method` | время до заголовков ответа S3 |
| `dumbwaiter_tls_certificate_expiration_timestamp_seconds` | gauge | `cert` | `NotAfter` загруженного сертификата в unix time |
| `dumbwaiter_tls_certificate_reloads_total` | counter | | перечитывания, после которых сертификаты сменились |
| `dumbwaiter_tls_certificate_reload_errors_total` | counter | | перечитывания с ошибкой, прокси остался на старых сертификатах |
| `dumbwaiter_build_info` | gauge | `version`, `goversion` | всегда 1 |

Рядом лежат стандартные `go_*` и `process_*`.

`listener` принимает значения `http`, `https` и `http3`, `proto` — `HTTP/1`, `HTTP/2` и `HTTP/3`, `method` — `GET`, `HEAD` и `OTHER`. В `host` попадают только хосты из конфига, запросы с любым другим `Host` идут как `unknown`, иначе сканер с выдуманными доменами раздул бы число серий. `code` у S3 равен статусу ответа, а если ответа не было — `error`, `timeout` или `canceled`. `canceled` значит, что клиент ушёл раньше, чем ответил S3: такие запросы считаются, но в гистограмму не попадают, потому что ничего не говорят о задержке хранилища.

`dumbwaiter_http_request_duration_seconds` меряет ответ целиком, поэтому большой файл, отданный медленному клиенту, растягивает хвост. Задержку самого хранилища честнее смотреть по `dumbwaiter_s3_request_duration_seconds`. `502` в `dumbwaiter_http_requests_total` почти всегда означает проблему с S3, подробности с кодом ошибки S3 лежат в логе.

В Helm сбор включается одним из флагов:

- `metrics.podMonitor.enabled` создаёт `PodMonitor` для Prometheus Operator. VictoriaMetrics Operator по умолчанию сам превращает его в `VMPodScrape`, если в кластере стоят CRD `monitoring.coreos.com`;
- `metrics.vmPodScrape.enabled` создаёт `VMPodScrape` для VictoriaMetrics Operator без CRD Prometheus;
- `metrics.scrapeAnnotations` вешает на pod'ы аннотации `prometheus.io/scrape`, `port` и `path` для сбора по аннотациям.

В kustomize-манифестах готового объекта нет, `PodMonitor` выглядит так:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: dumbwaiter
spec:
  podMetricsEndpoints:
    - port: admin
      path: /metrics
  selector:
    matchLabels:
      app.kubernetes.io/name: dumbwaiter
```

Несколько запросов для начала:

```promql
# доля 5xx по хостам
sum by (host) (rate(dumbwaiter_http_requests_total{code=~"5.."}[5m]))
  / sum by (host) (rate(dumbwaiter_http_requests_total[5m]))

# p99 задержки S3 по бакетам
histogram_quantile(0.99, sum by (bucket, le) (rate(dumbwaiter_s3_request_duration_seconds_bucket[5m])))

# сертификат истекает меньше чем через 14 дней
dumbwaiter_tls_certificate_expiration_timestamp_seconds - time() < 14 * 86400
```

## Сборка

`make` без аргументов показывает все цели. Основные:

```bash
make build          # бинарник в ./dist
make ci             # golangci-lint + тесты с -race, перед push
make ci-full        # плюс gofmt, go mod tidy, govulncheck, проверка всех конфигов, рендер overlay-ев, helm-lint и compose-config
make helm-lint      # helm lint чарта и dumbwaiter -check для конфига, который он рендерит
make compose-config # docker compose config для обоих вариантов
make docker-build   # образ ghcr.io/jtprogru/dumbwaiter:<git describe без v>
make set-version VERSION=1.2.0  # версия в чарте, kustomize base и compose перед релизом
make diff deploy OVERLAY=tls NAMESPACE=web
```

golangci-lint и govulncheck ставятся в `./bin` запиненных версий при первом вызове. Makefile совместим с GNU Make 3.81, который идёт в macOS.

Версия везде одна и без `v`: бинарник (`-version`, метрика `dumbwaiter_build_info`), образ, версия и `appVersion` чарта, архивы и заголовок релиза. Букву `v` несёт только git-тег `v1.2.0`, этого требуют Go-модули. Чарту `v` запрещает и сам Helm: такая версия не проходит SemVer2, и без явного `--version` Helm её не находит.

## Релизы

Релиз собирает `.github/workflows/release.yaml` по тегу `vX.Y.Z` на коммите из `main`. Перед тегом `make set-version VERSION=X.Y.Z` выставляет версию в файлах репозитория, иначе workflow выдаст предупреждение (артефакты всё равно получат версию из тега). Сначала прогоняется весь CI, затем публикуются архивы с бинарниками для linux и darwin, образ `ghcr.io/jtprogru/dumbwaiter:X.Y.Z` для amd64 и arm64 и чарт `oci://ghcr.io/jtprogru/charts/dumbwaiter` версии `X.Y.Z`. Release notes берутся из сообщения аннотированного тега, без него GoReleaser соберёт их из коммитов. Релиз вручную создавать не нужно, его делает workflow.

У каждого артефакта есть подписанная attestation, образ и чарт дополнительно подписаны cosign. Команды проверки есть в описании каждого релиза.

Образ собирается на `golang:1.27` и запускается на `gcr.io/distroless/static-debian12:nonroot`. Порты: 8080 (HTTP), 8443/TCP и 8443/UDP (HTTPS и HTTP/3), 9090 (admin).

## Чего нет и не планируется

Кэша в прокси нет: S3 и браузерный кэш с `ETag` справляются, а для серьёзной нагрузки перед прокси лучше поставить CDN. Трейсинга нет, есть метрики и access-лог в JSON (`log.access: false` его выключает, метрики продолжают считаться). Wildcard-домены не поддерживаются.
