# Локальный мониторинг dumbwaiter

Самодостаточный dev-стенд по образцу `cubic-root`: vmagent → VictoriaMetrics,
Vector → VictoriaLogs, Grafana и vmalert. Production-конфиги в `deploy/compose`
не меняются. Приложение собирается из текущего checkout.

## Запуск

Нужны Docker Engine/Desktop с Compose v2, Python 3 и `make`. Для скачивания
образов и первого запуска Grafana нужен Интернет: Logs datasource plugin
устанавливается синхронно, его версия закреплена. Отдельная установка Go для
`obs-*` не требуется — приложение собирается внутри Docker.

Из корня репозитория:

```sh
make obs-check  # валидация Compose, vmagent, Vector, vmalert, unit-тесты алертов
make obs-up     # сборка приложения и запуск
make obs-smoke  # дождаться готовности и проверить весь путь данных
```

| Сервис | URL | Назначение |
|---|---|---|
| Grafana | http://localhost:3333 | `admin` / `admin`, папка `dumbwaiter` |
| dumbwaiter | http://localhost:8080 | локальный сайт; Host должен быть `localhost` |
| Admin | http://localhost:9090/metrics | метрики, `/healthz`, `/readyz` |
| VictoriaMetrics | http://localhost:8428/vmui | MetricsQL / PromQL |
| vmagent | http://localhost:8429/targets | состояние scrape targets |
| VictoriaLogs | http://localhost:9428/select/vmui | LogsQL |
| vmalert | http://localhost:8880 | pending/firing алерты; уведомления **не отправляются** |

Если порты заняты соседним проектом, экспортируй overrides **перед обеими командами**:

```sh
export APP_PORT=18080 ADMIN_PORT=19090 GRAFANA_PORT=13333
export VM_PORT=18428 VMAGENT_PORT=18429 VL_PORT=19428 VMALERT_PORT=18880
export GRAFANA_ADMIN_USER=admin GRAFANA_ADMIN_PASSWORD='local-test-password'
make obs-up
make obs-smoke
```

Smoke script использует экспортированные переменные, а не читает `.env`.
Изменение Grafana admin password в environment не меняет пароль уже созданной
БД в volume: настрой его через Grafana либо используй прежние credentials.

## Что запущено

```text
client / blackbox ──> dumbwaiter ──> nginx fixture (anonymous GET/HEAD)
                         │
                         ├── /metrics ──> vmagent ──> VictoriaMetrics <── vmalert
                         └── stdout ──> Vector ──> VictoriaLogs
                                                       ↑
                               Grafana queries both storages
```

`fixture` — статический HTTP-сервер с path-style адресами `/demo/key`, **не S3**:
нет IAM, SigV4, XML AccessDenied и реалистичного поведения хранилища. Он нужен,
чтобы сразу получить воспроизводимые метрики и логи без секретов и внешних
бакетов. `/upstream-error.txt` отдаёт upstream 503, который dumbwaiter превращает
в 502. Production-настройки приложения не затрагиваются.

Blackbox каждые 15 секунд делает GET `/probe.txt` через публичный listener,
проверяет `200` и точное содержимое. Нет SPA/custom-404 fallback. Это проверка
локальной цепочки proxy → fixture, **не доступности из Интернета и не SLO**.
Её запросы учитываются в HTTP/S3 counters; в локальном конфиге host `dumbwaiter`
позволяет отделить их от ручных запросов с host `localhost`.

## Дашборды

| Дашборд | На какой вопрос отвечает |
|---|---|
| Overview / RED | Есть ли трафик, 5xx, обрывы, высокая полная latency, отказы admission? |
| S3 dependency | Где задержка до заголовков, таймауты, misses, masked AccessDenied? |
| Runtime / TLS | CPU в ядрах, RSS/heap, goroutines, FD utilization, GC, срок сертификатов |
| Logs | Что произошло с запросом: status, outcome, path, duration_ms; ошибки доставки логов |
| Monitoring health | Работают ли scraper/хранилища/rules, растут ли очереди и потери логов? |
| Load test / k6 | Доставлен ли целевой RPS, чем отличаются клиент/сервер, где задержка и saturation? |

Переходы между дашбордами сохраняют время и общие переменные. HTTP-панели
фильтруются по instance/host/listener, S3 — по instance/bucket. Панели без
соответствующих labels явно подписаны: например, admission не имеет listener,
а in-flight — host. Logs и Monitoring health показывают весь локальный стек.
В Logs детали строки раскрывают структурированные поля; для произвольного
фильтра используй Explore с datasource VictoriaLogs:

```text
{service="dumbwaiter",environment="local"} status:="502"
{service="dumbwaiter",environment="local"} outcome:in("aborted", "write_error", "panic")
{service="dumbwaiter",environment="local"} path:="/probe.txt"
```

**Не путай пустую панель с нулём:** lazy counters появляются после первого
события, TLS отсутствует при plain HTTP, квантиль без наблюдений не определён.
Доля ошибок равна нулю при существующем трафике без ошибок; отсутствие
телеметрии и отсутствие трафика не превращаются в «100% успешно».
HTTP latency включает передачу тела, а `200` не гарантирует её успех.
Подробности: [аудит метрик](metrics-audit.md).

JSON генерируется из `scripts/monitoring-dashboards.py`, но хранится в git:

```sh
make obs-dashboards  # после изменения генератора
make obs-check      # также проверяет, что JSON не устарел
```

Используется встроенный Grafana Prometheus datasource для VictoriaMetrics
(без дополнительного metrics plugin); LogsQL требует закреплённого плагина
VictoriaLogs. Grafana автоматически подхватывает изменения JSON примерно за
10 секунд. Изменение datasource provisioning требует перезапуска Grafana.

## Нагрузка k6: фоновая и ручная

Контейнер `grafana/k6:2.4.0`, установка k6 на хост не нужна. Нагрузка направлена
**только в локальный fixture** через `http://dumbwaiter:8080` с Host
`loadtest.local`. Это отдельный alias того же сайта/бакета. Он отделяет нагрузку
от blackbox (`dumbwaiter`) и ручных curl (`localhost`) в метриках и логах.
Существующий `scripts/loadtest.js` для произвольных объектов не меняется.

### Фоновый источник на 72 часа

```sh
make obs-load-up    # запускает/обновляет стек и отдельный k6-background
make obs-load-logs  # ID прогона, сообщения k6; Ctrl-C отключает только просмотр
make obs-load-down # останавливает только фоновую нагрузку
```

Обычный `make obs-up` **не запускает** нагрузку: сервис включается явно через
Compose profile `load`. Один проход длится 72 часа активной работы плюс setup
и до 12 секунд graceful stop. Каждый час повторяются шесть стадий по 10 минут:

| Стадия | Целевой RPS |
|---|---|
| 1 | плавно 10 → 50 |
| 2 | плавно 50 → 200 |
| 3 | плато 200 |
| 4 | плавно 200 → 80 |
| 5 | плавно 80 → 10 |
| 6 | плато 10 |

Executor `ramping-arrival-rate`, timeUnit=1s; одна итерация — один запрос,
редиректы и повторы в measured traffic выключены. Есть 100 предварительно
подготовленных VUs, максимум 400. **Целевой RPS не гарантирует достигнутый**:
при нехватке VUs k6 пишет dropped iterations, при аварии сервера растут ошибки.
Не добавляй `sleep()` внутрь итерации arrival-rate теста.

По завершении/ошибке контейнер не перезапускается автоматически; exit code
сохраняется. Новый `obs-load-up` после остановки запускает новый 72h проход с
новым ID, **не продолжает** предыдущий. Сон ноутбука, остановка Docker и нехватка
ресурсов нарушают непрерывность; для настоящего soak-теста нужен хост без сна.
`make obs-down` останавливает также оба k6-сервиса.

### Ручные профили через Make

Сначала `make obs-up`, затем:

```sh
make test-load-smoke   # 30s, 10 RPS, около 300 запросов
make test-load-steady  # 5m, 100 RPS, около 30 000 запросов
make test-load-spike   # 3m: низкое плато → ramp → 1m при 200 → ramp → низкое плато

# Эквивалентный общий entrypoint:
make test-load LOAD_PROFILE=spike
```

Команда работает в foreground, печатает k6 summary и возвращает ненулевой
код при нарушении thresholds. Ctrl-C прерывает прогон. Контейнер удаляется
после выхода, результаты в VictoriaMetrics остаются. Ручная нагрузка и фон
**складываются**: для изолированного сравнения сначала `make obs-load-down`.

Для быстрых проверок можно сократить только время ручного профиля, не RPS:
`make test-load LOAD_PROFILE=spike LOAD_TIME_SCALE=0.1` (18 секунд).
Для background уменьшение времени запрещено. Короткие тесты имеют меньше
samples; rate/quantile на графике нужно интерпретировать с учётом окна.

### Смесь и критерии результата

На каждые 100 итераций: 85 GET известного файла, 10 HEAD, 4 ожидаемых 404,
1 ожидаемый 502 через `/upstream-error.txt`. Это детерминированная смесь для
диагностических графиков, **не модель production-пользователей**. Тела GET
проверяются целиком; правильный статус с оборванным/неверным body — ошибка.

k6 принимает ожидаемые 404/502 через per-request response callback, поэтому
`http_req_failed` означает неожиданную ошибку/transport failure, а не любую 5xx.
На серверном графике штатно будет около 1% 502. Сравнение двух кривых это
показывает, не скрывает. Любой admission 503, timeout или неверное тело нарушает
проверку. Readiness/setup запросы исключены из measured RPS и HTTP thresholds.

Thresholds: completed iterations >0, unfinished balance=0, dropped=0,
unexpected responses <0.1%, unexpected HTTP failures <0.1%. У ручных профилей
дополнительно p95<500ms, p99<1s. Это тестовые budgets локального fixture,
**не утверждённые SLO**. Нет `abortOnFail`: фон продолжает давать трафик во время
экспериментов, итоговый код показывает нарушение correctness/delivery thresholds.

### Что смотреть в Grafana

[Load test / k6](http://localhost:3333/d/dumbwaiter-load) использует подход из
`cubic-root · Load test`: план/клиент/сервер, ошибки, фазы запросов и saturation.

- `profile` и `testid` выбирают профиль и отдельный прогон; ID создаётся один раз
  в entrypoint и одинаков для всех VUs. Разные процессы не пишут в одну серию.
- Planned vs achieved RPS: сумма целевого rate и клиентского completion rate
  выбранных прогонов рядом с общим серверным RPS для Host `loadtest.local`.
  Отдельная панель показывает вклад каждого profile/testid.
- p99 клиент/сервер и p95 фаз blocked/connecting/sending/waiting/receiving.
  Клиентский `http_req_duration` = send+wait+receive; connect/blocked туда не входят.
  **Разность двух p99 не равна сетевой latency**, поэтому такого вычитания нет.
- Ошибки: rolling доля неожиданных HTTP результатов k6 рядом с серверными 5xx;
  отдельно cumulative доля неверного status/body с начала прогона.
- Dropped iterations, VUs, свежесть последнего sample, throughput и серверные
  CPU/RSS/in-flight/admission rejections. Есть переход в Runtime / TLS.

У серверных метрик нет testid/profile — они **не фильтруются** выбором отдельного
k6 прогона. Серверный RPS/latency объединяет все нагрузочные прогоны на этом Host,
а runtime — вообще весь процесс. Не добавляем уникальный testid в labels
приложения ради корреляции: это бесконтрольный рост cardinality. Для чистого
сравнения запускай один прогон или выбирай все параллельные прогоны и учитывай
их суммарный RPS.

### Память, доставка и дисковое пространство

k6 отправляет результаты напрямую в VictoriaMetrics `/api/v1/write` каждые
5 секунд; отдельный scrape target для k6 не нужен. Используются **native
histograms** (`K6_FEATURES=native-histograms`), которые VictoriaMetrics хранит
в `vmrange` buckets. Дашборд считает rolling quantiles по buckets, а не усредняет
готовые p99. Для 72h режима выключены Trend thresholds и end-of-test summary:
они потребовали бы хранения миллионов сырых latency samples. Rate/counter
thresholds остаются включёнными. У коротких профилей подробный summary сохранён.

Версия k6 закреплена: её `experimental-prometheus-rw` deprecated в пользу OTLP,
но проверена с этим стеком. Stale markers выключены: k6 2.4 с native histograms
в финальном batch смешивает samples/histograms, а VM 1.156 отвергает такой batch.
Поэтому после завершения серии исчезают по обычной staleness, не мгновенно;
смотри age последнего sample и статус контейнера. Remote write не заменяет
надёжную очередь: при недоступной VM данные могут теряться. Ошибки output
проверяются отдельно от exit code k6 в `obs-load-check`.

72h волна — около **23.76 млн запросов** плюс upstream error logs. Это много
гигабайт сырых access-логов; фактический сжатый объём зависит от данных. Следи
за диском Docker: 7-дневный retention не ограничивает объём. Небольшие fixture
объекты не моделируют большие скачивания/медленных клиентов.

Проверки:

```sh
make obs-check       # JS contract tests, inspect всех профилей; без нагрузки
make obs-load-check  # реальный smoke 30s + укороченные steady/spike, ~80s
make obs-smoke       # все provisioned dashboard queries, включая k6
```

`obs-load-check` проверяет thresholds, смесь статусов, target RPS, доставку
метрик и реальный quantile из native histogram. CI запускает эти же проверки,
но **не** фоновый 72h прогон. Полные 72 часа не подменяются коротким тестом.

## Проверки и эксперименты

`make obs-smoke` проверяет реальные ответы 200/404/405/502, ограничение cardinality
неизвестных Host, поступление свежего структурированного лога, 9 scrape targets,
синтетику, datasource health, наличие 6 дашбордов и выполнение всех их запросов.
Logs-запросы выполняются **через Grafana plugin**. Ошибочный запрос ломает тест;
пустые результаты TLS/lazy counters допустимы. Скрипт генерирует небольшой
объём тестового трафика, не нагрузку. CI запускает тот же стек и проверки.

Ручные запросы (подставь другой APP_PORT, если изменён):

```sh
curl -i http://localhost:8080/probe.txt
curl -i http://localhost:8080/missing.txt
curl -i http://localhost:8080/upstream-error.txt
```

Для проверки реакции на отказ **только локального fixture**:

```sh
docker compose -f deploy/monitoring/compose.yaml stop fixture
# Через scrape/evaluation delay + 1 минуту DumbwaiterKnownObjectFailed firing.
# При этом up{job="dumbwaiter"} остаётся 1: процесс жив, пользовательский путь сломан.
docker compose -f deploy/monitoring/compose.yaml start fixture
# Probe и alert восстановятся после следующих scrape/evaluation.
```

Алерты: scrape target down, исчезновение метрик приложения/синтетики, провал
known-object, сертификат менее 7 дней, ошибки перечитывания TLS. Pending окна
и восстановление проверяются unit-тестами `alerts/tests.yaml`. Отсутствие TLS
в локальном HTTP-режиме не вызывает тревогу. Ожидаемые TLS targets в production
нужно контролировать отдельно: `absent()` для произвольного набора сертификатов
не заменяет inventory. Нет произвольных latency/5xx порогов, SLO burn-rate или
Alertmanager; они требуют согласования SLI/целей и маршрута уведомлений.

## Безопасность, хранение, ограничения

- Все опубликованные порты привязаны к `127.0.0.1`. Admin/VM/VL/vmagent/vmalert
  не имеют настроенной аутентификации. Не публикуй их в Интернет.
- **Docker socket даёт Vector высокопривилегированный доступ к Docker daemon.
  Mount `:ro` не запрещает изменяющие вызовы Docker API.** Этот вариант допустим
  только на доверенной локальной машине. Для production нужны отдельный
  log collector или ограниченный Docker API proxy. Фильтр labels ограничивает
  собираемые логи проектом `dumbwaiter-monitoring` и сервисом `dumbwaiter`, но
  не является границей доступа. Не переименовывай Compose project через `-p`
  без изменения фильтра.
- Access logs содержат path, Host, remote address, user-agent. Query string не
  записывается; путь тоже может содержать чувствительные данные. Нужны redaction
  и правила доступа до подключения настоящего пользовательского трафика.
- Метрики/логи хранятся 7 дней, volumes переживают `obs-down`. Retention по времени
  **не ограничивает объём диска**. Docker logs ротируются (3 × 10 MB на контейнер),
  очереди vmagent/Vector ограничены примерно 256 MiB каждая.
- Vector использует disk buffer после чтения, но Docker log source не даёт
  exactly-once/replay гарантии: при остановке collector часть логов можно потерять.
  Приложение стартует после готовности Vector, чтобы не терять startup logs.
- Нет cAdvisor/node exporter: RSS и CPU — процессные метрики, не процент лимита
  контейнера; disk/node pressure требуют отдельного мониторинга хоста.
- TLS/HTTP2/HTTP3, IAM, медленные клиенты и saturation не моделируются default
  fixture. Соответствующие метрики есть на дашбордах, но это не end-to-end тест
  этих сценариев; для них остаются Go integration tests и load tests.

Остановить без удаления данных:

```sh
make obs-down
```

Полностью удалить **локальные метрики, логи, Grafana DB и очереди этого стека**
(необратимо; запускай только когда история больше не нужна):

```sh
docker compose -f deploy/monitoring/compose.yaml --profile load --profile tools down --volumes
```

## Если данных нет

1. `docker compose -f deploy/monitoring/compose.yaml ps` и `logs --tail=100`.
2. Открой vmagent `/targets`: DNS/connection errors отличаются от пустой серии.
3. Для Grafana проверь загрузку Logs plugin и datasource health; первый запуск
   зависит от доступности каталога плагинов.
4. Для логов проверь Vector health и точные Compose labels. Не задавай `-p`.
5. Времена scrape 15s, latency offset 5s, batching логов 1s: один curl не обязан
   немедленно отобразиться в графике `rate()`. Для нескольких samples подожди минуту.
