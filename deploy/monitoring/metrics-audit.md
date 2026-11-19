# Аудит метрик dumbwaiter

Аудит исходного кода и live `/metrics` локального Linux-контейнера. База:
`83a84a2` (0.3.0), плюс изменения мониторинга в этой ветке. Не нагрузочное
исследование и не утверждение о production SLO.

## Краткий вывод

Для диагностики RED уже достаточно метрик: счётчики запросов, latency,
исходы streaming, throughput, in-flight, admission rejections и S3. Labels
в основном ограничены конфигурацией, а гистограммы совместимы с PromQL.

**Нельзя честно построить download-success SLO только по текущим counters**:
HTTP status и outcome не объединены в одной серии. HTTP latency не отделяет
TTFB от передачи тела и не разделяется по классу ответа. Saturation видна по
отказам, но процент занятых admission-слотов вычислить нельзя.

В этом изменении исправлен подтверждённый пробел FD-метрик и добавлен внешний
known-object probe локального пути. Семантика существующих прикладных метрик
не менялась. Остальные изменения требуют отдельной инструментации.

## Источники в коде

- `internal/metrics/metrics.go` — все имена, labels, buckets, экспорт runtime.
- `internal/server/observe.go` — начало/конец HTTP, статус, байты, outcome,
  access log, обработка abort/panic.
- `internal/s3/client.go` — замер до заголовков, классификация transport errors,
  отдельный idle timeout чтения тела.
- `internal/proxy/handler.go` и `lookup.go` — admission, несколько lookup на
  один HTTP request, AccessDenied masking, streaming abort.
- `internal/tlsstore/store.go` — expiration и reload.
- `github.com/VictoriaMetrics/metrics v1.45.0` — фактический набор `go_*` /
  `process_*`; не нужно предполагать, что он совпадает с client_golang.

## Инвентаризация

`C` — counter, `G` — gauge, `H` — classic histogram. У всех собранных серий
vmagent добавляет `job` и `instance`; они не являются labels приложения.

| Имя (`dumbwaiter_` prefix) | Тип, labels приложения | Интерпретация / ограничение |
|---|---|---|
| `http_requests_total` | C: listener, proto, host, method, code | Завершившиеся handler calls, включая ошибки/abort; code=0, если статус не был передан writer. Не arrival rate при длинных запросах |
| `http_request_duration_seconds` | H: listener, host | Весь handler + body/flush, все status/outcome вместе. Не TTFB и не latency успешных ответов |
| `http_response_bytes_total` | C: listener, host | Байты, принятые локальным writer, не доказательство получения клиентом |
| `http_requests_in_flight` | G: listener | Текущие публичные запросы, включая ещё не прошедшие admission; не занятые slots |
| `http_response_outcomes_total` | C: listener, host, outcome | complete / canceled / write_error / aborted / panic, отдельно от status |
| `admission_rejected_total` | C: host, scope | global/site отказ до S3; оба ограничения действуют во время streaming |
| `lookup_timeouts_total` | C: host | Общий бюджет выбора объекта, не длительность одного S3 запроса |
| `s3_requests_total` | C: bucket, method, code | Один HTTP request может дать несколько S3 attempts; code=status либо error/timeout/canceled |
| `s3_request_duration_seconds` | H: bucket, method | До заголовков (или ошибки); canceled исключён. Ошибки чтения body сюда не входят |
| `s3_access_denied_masked_total` | C: bucket | 403 AccessDenied → 404. Без ListBucket так же выглядит обычный miss; не самостоятельный IAM alert |
| `tls_certificate_expiration_timestamp_seconds` | G: cert | NotAfter загруженного сертификата; `cert` — конфигурационный путь |
| `tls_certificate_reloads_total` | C: — | Успешная замена сертификатов |
| `tls_certificate_reload_errors_total` | C: — | Перечитать не удалось, старый сертификат остаётся |
| `build_info` | G: version, goversion | 1; версия нужна для корреляции с релизом, не как label каждой метрики |

Прикладные counters/histograms в основном **lazy**: нет серии, пока не произошло
событие. In-flight создаётся для включённых listeners при старте, TLS counters —
при инициализации TLS. Поэтому пустая TLS-панель в HTTP-only стенде ожидаема.

### Гистограммы

Finite buckets (секунды): `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
5, 10, 30`, плюс `+Inf`, `_count`, `_sum`: **15 серий на комбинацию labels**.
Это classic `le`, не `vmrange` и не native histograms.

- Сначала `rate()` для каждой серии, затем суммирование buckets с сохранением
  `le`, затем `histogram_quantile()`. Не усреднять p99 разных реплик.
- Для p99, попавшего в `+Inf`, classic quantile может вернуть границу 30s —
  это не реальное измерение длинного хвоста. Overview показывает долю `>30s`.
- S3 p99 по нескольким попыткам не эквивалентен p99 выбора ответа целиком.
- Latency SLO требует согласованного события, порога и bucket точно на пороге.
  Текущий full-body histogram нельзя молча использовать как SLO TTFB.

### Cardinality и приватность

Хорошо:

- `host` — configured aliases или `unknown`, не произвольный клиентский Host;
- HTTP `method` — GET/HEAD/OTHER, `proto` и `listener` ограничены;
- path, query string, user-agent, remote IP, object key отсутствуют в labels;
- bucket и cert берутся из конфигурации, outcome/scope из фиксированного набора.

Ограничения:

- Число серий растёт с количеством aliases × listeners × statuses × replicas.
  Это ограничение конфигурацией, а не глобальный фиксированный предел.
- Одинаковое имя bucket на разных S3 endpoints сейчас сливается в одну серию.
  Для multi-backend диагностики нужен стабильный конфигурационный `backend_id`,
  не URL с credentials и не object key.
- Host из access log **сырой**, в отличие от metric label. Он не должен стать
  stream key в VictoriaLogs. Stream keys здесь: service/environment/container_name.
- Сертификатный путь/названия bucket/домены видны на неаутентифицированном `/metrics`.
  Ограничение admin на уровне сети обязательно.

## Подтверждённая исправленная проблема

### FD utilization отсутствовала

`Metrics.Handler()` вызывал `vm.WriteProcessMetrics`, но в версии v1.45.0 этот
метод **не** вызывает `WriteFDMetrics`. Живой `/metrics` не содержал
`process_open_fds`/`process_max_fds`.

Исправление: отдельный `vm.WriteFDMetrics(w)` в handler. Regression-тест
`TestHandlerExportsFileDescriptorMetrics` сначала воспроизвёл отсутствие обоих
рядов, затем проверил положительные значения и отсутствие duplicate series
на Linux/macOS. FD collection перечисляет дескрипторы при scrape: при очень
большом количестве FD контролируй стоимость через `scrape_duration_seconds`.

## Пробелы и следующий этап инструментации

| Приоритет | Пробел | Что добавить / критерий приёмки |
|---|---|---|
| P1 | Нет совместной классификации status × outcome | Один счётчик завершений с bounded результатом (`success`, `http_error`, `transfer_error`, `canceled`, политика согласована) либо bounded status_class + outcome. Один запрос учитывается ровно один раз; тесты 200+abort, 502+complete, cancel, panic, code=0 |
| P1 | In-flight не даёт admission utilization | Gauges занятых slots и capacity по global/site со стабильным site ID; aliases не дублируют общий лимит. Occupancy никогда не превышает capacity, возвращается к 0 после abort |
| P1 | Нет клиентского TTFB / latency успешных ответов | Отдельный замер выбора/начала ответа с bounded outcome/status_class, согласованные buckets. Body time оставить отдельной метрикой; проверить медленный upstream и медленного клиента |
| P1 для нескольких endpoints | S3 bucket не уникален между backends | Конфигурационный backend ID, без секретов; тест двух одинаковых bucket names на разных endpoints |
| P2 | Причины body abort плохо разделены | Bounded upstream read timeout / upstream read error / downstream write failure; не включать текст ошибки в label. Уже существующий outcome сохранить совместимым |
| P2 | Нельзя явно проверить весь ожидаемый inventory TLS/probes | Expected-target inventory и missing-series alerts по конкретному backend/cert; отсутствие данных отличать от выключенного TLS |
| P2 | Логи не связывают access и upstream event уникальным ID | Request ID как log field, не metric label; политика доверия входному ID и redaction чувствительных path |

Не включены container quota, disk saturation и сетевые метрики хоста: они
принадлежат инфраструктурным collectors. Не подменять CPU cores процентом
контейнерного лимита, если сам лимит неизвестен.

## Что можно и нельзя считать сейчас

**Можно:** HTTP 5xx ratio, долю локально не завершённых transfers, долю client
cancellations, S3 error/timeout rate, histogram latency, admission reject rate,
ресурсные тренды, работоспособность known object.

**Нельзя:** сложить 5xx ratio и failed-transfer ratio (пересечение), вычесть их
сумму из 1 и назвать availability; считать complete доказательством получения
байтов клиентом; считать все 403/404 ошибкой S3; считать отсутствие серии нулём;
считать readiness или `up=1` пользовательской доступностью.

Дашборды используют zero fallback только относительно существующего total с
теми же labels: `(errors or 0 * total) / (total > 0)`. Это устраняет пустой
числитель lazy counter, но не маскирует отсутствие трафика или scrape. Нет
`or vector(0)` вокруг всей формулы и нет преобразования No data в healthy.

Для production SLO сначала согласовать eligible requests (GET/HEAD? unknown
host? 4xx? canceled?), определение успеха, TTFB/full body/размер объекта, окно
оценки и цель. Затем добавить recording rules и multi-window burn-rate alerts.
Семидневный локальный retention не является 30-дневным отчётом SLO.

## Проверка реализации

- `make obs-check`: Compose, vmagent, Vector, vmalert, alert unit tests,
  воспроизводимость dashboard JSON.
- `make obs-smoke`: end-to-end ответы, scrape, Logs ingestion с сохранённым
  HTTP Host/status, synthetic status+body, datasource health, provisioning и
  выполнение всех dashboard queries, zero/error/no-data semantics.
- `go test -race ./...`, `go vet ./...`, `make lint`: regression и существующие
  проверки инструментации HTTP/streaming/TLS.
- Smoke не доказывает корректность всех latency распределений под нагрузкой;
  TLS/HTTP3, admission saturation и slow transfer надо проверять отдельно.

## Основания и источники

- [Google SRE: Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/):
  latency, traffic, errors, saturation; black-box вместе с white-box, симптомы
  отдельно от причин.
- [SRE Workbook: Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/):
  burn rate применяется к определённому SLI/SLO, а не к произвольному порогу p99.
- [Prometheus: Histograms and summaries](https://prometheus.io/docs/practices/histograms/):
  агрегация buckets и ограничения quantiles.
- [VictoriaLogs: Vector ingestion](https://docs.victoriametrics.com/victorialogs/data-ingestion/vector/):
  Elasticsearch sink, `_msg_field`, `_time_field`, низкокардинальные stream keys.
- [VictoriaLogs: Grafana integration](https://docs.victoriametrics.com/victorialogs/integrations/grafana/):
  datasource provisioning и типы LogsQL queries.
- `../cubic-root/compose.yaml` и `observability/` послужили локальным примером
  структуры; его SLO/trace instrumentation не переносились в другой домен.
