# Архитектура `orpheus-gitlab-mr-review`

## 1. Назначение документа

Этот документ описывает целевую архитектуру автоматического ревью GitLab merge request в
распределённом контуре Orpheus Cloud. Он фиксирует не только границы нового сервиса
`orpheus-gitlab-mr-review`, но и полный путь данных от назначения reviewer в GitLab до публикации
результатов и удаления reviewer.

Документ должен позволять восстановить:

- зачем существует каждый компонент;
- где выполняется агент и где принимаются управляющие решения;
- какие данные являются durable state;
- как создаётся, завершается и отменяется ревью;
- как review artifacts покидают одноразовую VM, не превращая Orpheus в файловое хранилище;
- как обеспечиваются идемпотентность и восстановление после рестарта;
- чем отличаются повтор выполнения агента и безопасный retry внешней операции;
- какие ограничения являются осознанными и что обязательно исправить до версии 1.0.

Здесь описано согласованное целевое поведение. Конкретные имена Go-типов, переменных окружения и
полей JSON могут уточняться при реализации, но семантика протоколов и границы ответственности не
должны меняться без отдельного архитектурного решения.

Ключевые слова **ДОЛЖЕН**, **НЕ ДОЛЖЕН**, **СЛЕДУЕТ** и **МОЖЕТ** используются в нормативном
смысле.

## 2. Контекст системы

### 2.1. От metal-монолита к cloud-контуру

В metal-версии Orpheus процесс ревью, Codex, локальный workspace, GitLab adapter и
оркестрационное состояние находились рядом в одном процессе и на одном сервере. Runtime мог читать
файлы, созданные агентом, непосредственно из workspace и затем публиковать их в GitLab.

В cloud-версии эти обязанности разделены:

```text
GitLab
  ↕
orpheus-gitlab-mr-review (connector)
  ↕ HTTP
Orpheus API ↔ PostgreSQL
  ↕
Orpheus worker ↔ AgentBox VM/sandbox ↔ Codex
  ↕
Orpheus Web UI
```

AgentBox является изолированным местом исполнения. Его локальная файловая система недоступна
connector-у. После окончания одноразовой session sandbox удаляется. Следовательно, прежняя схема
«runtime читает `.orpheus/reviews/...` с локального диска» больше неприменима.

Для MR review действует следующая концептуальная эквивалентность:

```text
одна reviewer assignment
  → одна Orpheus session
  → один Orpheus run
  → одна одноразовая AgentBox VM
  → один Codex thread
```

Session создаётся с `allow_multiple_runs=false`. Возобновление VM и второй run в этой session не
допускаются. Новое ручное назначение reviewer после окончания или ошибки создаёт новую независимую
session.

### 2.2. Основной принцип

Connector управляет жизненным циклом ревью и всеми изменениями GitLab. Агент только анализирует
код и пишет локальные review artifacts. Между ними находится детерминированный helper, который
валидирует artifacts и формирует конечный publication bundle.

```text
GitLab-review connector
  → создаёт одноразовую Orpheus session с before_run/after_run
  → агент пишет findings в workspace AgentBox
  → after_run запускает доверенный helper
  → helper валидирует artifacts и печатает компактный JSON envelope
  → Orpheus сохраняет stdout after_run в PostgreSQL как HookResult
  → connector читает HookResult
  → connector повторно проверяет актуальный GitLab diff
  → connector идемпотентно управляет discussions, notes и reviewer
```

Review-файлы не загружаются в Orpheus как файлы и не копируются в connector. Из VM выходит только
проверенный, ограниченный по размеру publication bundle, необходимый для GitLab mutations.

## 3. Цели и нецели

### 3.1. Цели

- Сохранить пользовательское поведение metal GitLab MR review при переносе исполнения в Orpheus
  Cloud.
- Сделать connector тонким владельцем polling, orchestration и GitLab mutations.
- Выполнять каждое ревью в новой одноразовой AgentBox VM.
- Исключить зависимость протокола публикации от текста финального ответа агента.
- Не передавать файловые artifacts через отдельный artifact API Orpheus.
- Переживать рестарт connector-а без его собственной БД.
- Не публиковать результат для устаревшего diff.
- Не запускать агента повторно после ошибок.
- Сделать каждую GitLab mutation безопасной для повторения после сетевой неопределённости.
- Сохранить возможность диагностики через Orpheus Web UI.

### 3.2. Нецели

- Connector не является универсальным workflow engine.
- В сервисе нет workflow catalog, динамической загрузки workflow или SIGHUP reload.
- Connector не хранит workspace, исходный код проекта или произвольные файлы из AgentBox.
- Orpheus core не получает специальный GitLab API и не знает семантику findings/discussions.
- Агент не публикует notes, discussions, approvals и не меняет reviewers.
- Автоматическое ревью не является GitLab approve и не заменяет человеческую проверку бизнес-логики
  и архитектуры.
- Webhooks на первом этапе не используются; источником событий является только polling.
- Kubernetes не является архитектурным требованием. Сервис может запускаться в Kubernetes, но его
  протоколы и корректность не должны зависеть от Kubernetes API.
- Повторный analysis run после ошибки, timeout или потери VM не выполняется.

## 4. Компоненты и границы ответственности

### 4.1. GitLab

GitLab является:

- источником MR, diff refs, description, notes, discussions и reviewer assignments;
- внешней системой, в которой виден пользовательский результат;
- durable-хранилищем опубликованных результатов и скрытых idempotency markers;
- источником истины о том, открыт ли MR и назначен ли bot-reviewer.

GitLab не является очередью задач. Connector выводит необходимость запуска из текущего состояния MR,
значимых activity notes и отсутствия соответствующего завершённого результата.

### 4.2. `orpheus-gitlab-mr-review`

Connector владеет всей GitLab-специфичной оркестрацией:

- периодически ищет MR, назначенные настроенному bot-reviewer;
- загружает MR, project, notes, discussions и diffs;
- вычисляет identity и fingerprints;
- определяет, нужно ли запускать новое ревью;
- рендерит единственный встроенный workflow и MR prompt;
- создаёт Orpheus session с one-shot policy;
- наблюдает за run и параллельно сверяет его с текущим состоянием MR;
- отменяет run при наступлении доменного события отмены;
- читает и валидирует `after_run` HookResult;
- публикует findings, recommendations, resolutions и completion note;
- публикует краткое сообщение об ошибке;
- снимает только своего reviewer;
- выполняет lifecycle retries внешних операций;
- восстанавливает работу после рестарта, используя Orpheus и GitLab.

Connector **не**:

- запускает Codex локально;
- получает доступ к AgentBox filesystem;
- интерпретирует финальный prose-ответ агента как результат ревью;
- доверяет агенту формирование JSON;
- передаёт агенту полномочия управлять GitLab discussions;
- хранит собственное durable orchestration state.

### 4.3. Orpheus API и PostgreSQL

Orpheus предоставляет общий execution API:

- атомарно создаёт session, первое message и первый run;
- сохраняет immutable session configuration;
- хранит message metadata, которую не передаёт агенту;
- хранит статусы run, hook results, историю и диагностику;
- принимает cancel для активного run;
- предоставляет данные Orpheus Web UI;
- координирует worker и AgentBox.

Orpheus не валидирует GitLab review schema и не выполняет GitLab mutations. Для Orpheus output
`after_run` остаётся непрозрачным текстом.

### 4.4. Orpheus worker и AgentBox

Worker создаёт AgentBox по выбранному sandbox template, выполняет hooks и harness. AgentBox содержит:

- checkout MR на pinned head;
- Codex и нужные CLI/skills;
- локальный каталог review artifacts;
- установленную из embedded payload проверенную копию helper-а.

VM не reusable. После терминального состояния run и исполнения `after_run` Orpheus удаляет sandbox.
Именно поэтому `after_run` является последней точкой, в которой review artifacts ещё доступны.

### 4.5. Codex agent

Агент:

- получает developer instructions и отрендеренный MR prompt;
- проверяет только pinned diff;
- использует существующие discussions как контекст;
- немедленно записывает кандидаты в `findings/`;
- перепроверяет кандидатов и перемещает их в `confirmed/` или `rejected/`;
- пишет recommendations и resolution intents в соответствующие каталоги;
- завершает работу только при пустом `findings/`.

Финальный ответ агента предназначен только для session log. Он не является publication protocol.

### 4.6. Packaging helper

Helper — доверенная программа, доставленная connector-ом. Она:

- принимает ровно один аргумент: путь к корню artifacts текущего review;
- проверяет структуру каталогов и форматы файлов;
- отклоняет pending, неоднозначные или слишком большие результаты;
- строит JSON payload из валидированных данных;
- детерминированно сортирует и нормализует содержимое;
- сжимает payload;
- печатает в stdout один компактный versioned JSON envelope;
- пишет диагностический текст только в stderr;
- завершает процесс ненулевым кодом при любой ошибке.

Helper не вызывает GitLab и не принимает решений о публикации.

### 4.7. Orpheus Web UI

Web UI является диагностическим интерфейсом. В сообщении об ошибке connector публикует ссылку вида:

```text
<ORPHEUS_WEB_URL>/sessions/<session_id>
```

`ORPHEUS_WEB_URL` задаётся отдельно. Его нельзя выводить из API URL: публичный адрес UI и внутренний
адрес API могут не иметь общего origin или path prefix.

## 5. Идентичность и fingerprints

### 5.1. Канонический ключ MR

Для связывания GitLab и Orpheus используется канонический ключ:

```text
<normalized-gitlab-host>:<project-id>!<mr-iid>
```

Нормализация host удаляет окружающие пробелы и завершающий `/`, но не должна превращать разные
GitLab installations в один источник. `project_id` и `iid` используются вместо изменяемого project
path.

Отдельная универсальная сущность `source identity` не вводится. Для session достаточно:

- постоянного workflow namespace;
- канонического MR key;
- immutable GitLab user ID bot-reviewer;
- fingerprints входного snapshot.

Username reviewer-а хранится как диагностический атрибут, но authorization и ownership checks
выполняются по immutable user ID. Если username задан в конфигурации, при старте connector проверяет,
что authenticated GitLab user соответствует ему.

### 5.2. Diff fingerprint

`diff_fingerprint` определяет проверяемое состояние кода. Он вычисляется как SHA-256 канонического
JSON со следующими полями:

```json
{
  "version": 1,
  "gitlab_host": "https://gitlab.example.com",
  "project_id": 42,
  "iid": 17,
  "source_branch": "feature/x",
  "target_branch": "main",
  "diff_refs": {
    "base_sha": "...",
    "start_sha": "...",
    "head_sha": "..."
  }
}
```

Этот fingerprint:

- закрепляет exact diff, доступный агенту;
- входит в artifact path и publication markers;
- проверяется перед каждой GitLab mutation;
- при изменении делает активное ревью stale и приводит к cancel.

Неполные diff refs не допускают создания session.

### 5.3. Review fingerprint

`review_fingerprint` определяет полный смысловой вход ревью. Он включает:

- versioned diff payload;
- актуальное MR description;
- значимые activity notes.

Значимыми считаются:

- human resolvable notes;
- system notes о назначении reviewer;
- system notes об изменении resolution state, если они влияют на повторную проверку.

Не учитываются собственные notes bot-reviewer, иначе публикация результата сама бесконечно меняла бы
fingerprint. Activity сортируется детерминированно, например по note ID и нормализованному body.

Два fingerprints нужны по разным причинам:

- изменение `diff_fingerprint` отменяет уже выполняемый анализ как устаревший;
- `review_fingerprint` позволяет отличить новую ручную reviewer assignment или значимое обсуждение от
  уже обработанного входа даже при неизменном коде.

### 5.4. Workflow revision

Workflow revision вычисляется connector-ом из семантических embedded assets, включая как минимум:

- developer instructions;
- MR prompt template;
- helper bytes;
- artifact/bundle schema и renderer logic version.

Revision сохраняется в metadata и bundle. Оператор не задаёт её вручную.

Изменение revision:

- применяется только к новым ревью;
- не запускает ревью уже назначенного MR автоматически;
- не отменяет созданную session;
- не меняет session после атомарного создания;
- не мешает старой session завершиться со старой revision.

Новая версия connector-а обязана уметь дочитать и опубликовать поддерживаемые schema versions,
созданные предыдущей версией и всё ещё находящиеся в полёте.

## 6. Embedded workflow и конфигурация

### 6.1. Единственный workflow

В бинарнике существует один workflow — GitLab MR review. Каталога workflow нет. Изменение workflow
равно сборке и deployment новой версии connector-а.

Статические semantic assets встраиваются через `embed.FS` либо хранятся строковыми константами, если
это не ухудшает читаемость:

- developer instructions, перенесённые из metal;
- `merge_request.md` template, перенесённый из metal;
- исходный или готовый payload helper-а;
- bootstrap shell scripts для `before_run` и `after_run`;
- JSON schema versions и marker format versions.

Шаблон сохраняется отдельным embedded asset. Его загрузка, строгая проверка контекста и rendering
выполняются Go-кодом connector-а. Отсутствующее поле, неверный тип или ошибка template execution
являются ошибкой подготовки; best-effort rendering запрещён.

### 6.2. Developer instructions

Metal instructions переносятся по смыслу и адаптируются к новой границе:

- unattended execution, без вопросов пользователю;
- работа только в выделенном workspace;
- проверка только pinned diff;
- обязательный файловый контракт findings/confirmed/rejected/recommendations/resolutions;
- использование существующих discussions для дедупликации и resolution intents;
- правила Kubernetes compliance skills;
- запрет GitLab mutations;
- финальный ответ не является каналом публикации;
- после агента artifacts проверяет helper, а публикует connector.

Из metal-текста удаляется предположение, что локальный runtime сам читает workspace после run, и
упоминание analysis retry. Невалидный или незавершённый результат теперь завершает workflow ошибкой;
новый analysis run автоматически не создаётся.

### 6.3. Runtime configuration

Environment-specific конфигурация задаётся преимущественно переменными окружения. В бинарник нельзя
встраивать URLs окружения, tokens, project allowlist или profile names.

Целевой ENV-контракт приведён ниже. При реализации имена можно изменить только согласованно с
deployment-конфигурацией и пользовательской документацией; сама семантика обязательности является
частью архитектуры.

| Переменная | Обязательность | Назначение |
|---|---:|---|
| `ORPHEUS_BASE_URL` | да | origin/base path Orpheus API |
| `ORPHEUS_API_KEY` | да | bearer credential connector-а к Orpheus API |
| `ORPHEUS_WEB_URL` | да | публичный base URL для диагностических ссылок в MR |
| `GITLAB_BASE_URL` | да | base URL обслуживаемой GitLab installation |
| `GITLAB_TOKEN` | да | credential authenticated bot-а; до 1.0 также может передаваться sandbox для read-only работы |
| `GITLAB_REVIEWER_USERNAME` | нет | ожидаемый username; если задан, валидирует current user, но не заменяет immutable user ID |
| `GITLAB_PROJECT_PATHS` | нет | разделённый запятыми allowlist project paths; пустое значение означает все доступные проекты |
| `ORPHEUS_AGENT_PROFILE` | да | профиль авторизации/исполнения агента |
| `ORPHEUS_SANDBOX_TEMPLATE` | да | AgentBox template |
| `ORPHEUS_AGENT_MODEL` | нет | явный model override; отсутствие использует policy профиля/Orpheus |
| `POLL_INTERVAL` | нет | период polling в Go duration syntax |
| `MAX_CONCURRENT_REVIEWS` | нет | локальный предел одновременно активных sessions |
| `RUN_TIMEOUT` | нет | deadline одного Orpheus run |
| `HOOK_TIMEOUT` | нет | deadline каждого hook |
| `HTTP_TIMEOUT` | нет | deadline одиночного GitLab/Orpheus request |
| `SHUTDOWN_TIMEOUT` | нет | время на уже начатые HTTP calls после снятия readiness |
| `MAX_SESSION_REQUEST_BYTES` | нет | локальный fail-closed предел `CreateSession` request |
| `MAX_HOOK_OUTPUT_BYTES` | нет | connector/helper limit ниже либо равный server limit Orpheus |
| `MAX_BUNDLE_UNCOMPRESSED_BYTES` | нет | предел распакованного publication payload |
| `LOG_LEVEL`, `LOG_FORMAT` | нет | настройки structured logging |

Списки нормализуются: пробелы обрезаются, пустые элементы удаляются, дубликаты запрещаются. Duration
должны быть положительными. Byte limits и concurrency имеют безопасные ненулевые defaults и верхние
границы. URL разрешают только `http`/`https`, запрещают embedded credentials и нормализуют trailing
slash. Конфигурация читается на старте и считается immutable до завершения процесса; live reload не
поддерживается.

Конфигурация валидируется полностью до перехода процесса в ready. Неизвестное критичное значение,
отсутствие обязательного URL/token/profile или некорректный limit приводят к startup failure.

### 6.4. Секреты

GitLab и Orpheus credentials поступают через environment/secret injection и никогда не входят в
образ или embedded assets. Connector не пишет их в logs, message metadata, prompt или HookResult.

Для clone/read-only проверок sandbox в текущей версии может получать GitLab credential через
разрешённый Orpheus/AgentBox secret flow. На текущем этапе запрет GitLab mutations внутри VM
обеспечивается инструкциями агента, а не техническим разделением полномочий. Это осознанное
ограничение, подробно зафиксированное в разделе 17.

## 7. Создание review session

### 7.1. Polling кандидатов

Connector с заданным interval получает MR, в которых authenticated bot назначен reviewer-ом. Если
настроен `project_paths`, поиск ограничивается ими. Пустой список означает все проекты, доступные
token-у и поддержанные GitLab API query.

Один poll tick логически состоит из двух фаз:

1. reconciliation уже известных/восстановленных sessions и незавершённых lifecycle operations;
2. admission новых кандидатов при наличии capacity.

Reconciliation выполняется раньше admission. Это не позволяет после рестарта создать новую session
для MR, у которого сначала нужно отменить stale run или закончить удаление reviewer.

Временная ошибка GitLab list/fetch не является событием отмены или workflow failure. Tick
завершается с диагностикой и повторяется позже.

### 7.2. Eligibility

Новая session может быть создана, только если одновременно выполняются условия:

- MR находится в состоянии `opened`;
- MR не исключён согласованными правилами eligibility, включая draft policy;
- bot всё ещё присутствует среди reviewers;
- reviewer identity подтверждена immutable GitLab user ID;
- project входит в scope;
- diff refs полны;
- для этой reviewer assignment/review fingerprint нет активной или уже принятой Orpheus session;
- нет completion/error/skip lifecycle, который должен быть завершён раньше нового запуска;
- доступна concurrency capacity.

Одна и та же reviewer assignment создаёт не более одной session. Изменение binary/workflow revision
не делает кандидата новым.

### 7.3. Снимок входа

Перед созданием session connector загружает согласованный snapshot:

- MR и project;
- diff refs;
- notes, нужные для `review_fingerprint`;
- discussions с author/resolution/position;
- bot identity;
- trigger reason;
- Git clone URL и web URL MR.

Затем connector вычисляет fingerprints, создаёт artifacts path и рендерит prompt. Все данные session
фиксируются одним `CreateSession`; после принятия запрос не дополняется новой revision или новым
состоянием MR.

### 7.4. Orpheus request

Семантически запрос содержит:

```yaml
allow_multiple_runs: false
namespace: gitlab-mr-review
external_key: <normalized-host>:<project-id>!<iid>
input_fingerprint: <review-fingerprint>
configuration:
  agent:
    profile: <configured-profile>
    model: <optional-configured-model>
    instructions: <embedded-developer-instructions>
  sandbox:
    template: <configured-template>
  hooks:
    timeout_seconds: <configured-hook-timeout>
    before_run: <generated-bootstrap-and-prepare-script>
    after_run: <generated-validate-and-pack-script>
  limits:
    run_timeout_seconds: <configured-run-timeout>
messages:
  - text: <rendered-merge-request.md>
    external_key: <stable-review-input-key>
    metadata: <immutable-machine-contract>
env/env_from:
  <only the values required by before_run and after_run>
```

Первое message одновременно является реальной задачей агенту и местом хранения integration metadata.
Отдельное пустое или служебное message ради идентификации не создаётся. Orpheus сохраняет metadata в
PostgreSQL, но не передаёт её агенту.

`CreateSession` атомарно создаёт session, message и первый run. Connector применяет стабильный
`Idempotency-Key`, производный от MR key, reviewer ID и `review_fingerprint`, но не от текущей
workflow revision. Если HTTP response потерян, тот же запрос можно безопасно повторить или найти
принятый run. Это lifecycle retry создания, а не повтор анализа.

### 7.5. Machine metadata

Metadata versioned и содержит достаточно данных для reconciliation после полного рестарта
connector-а. Рекомендуемый логический состав:

```json
{
  "schema_version": 1,
  "workflow_id": "gitlab-mr-review",
  "workflow_revision": "sha256:...",
  "gitlab": {
    "host": "https://gitlab.example.com",
    "project_id": 42,
    "project_path": "group/project",
    "mr_iid": 17,
    "mr_url": "https://gitlab.example.com/group/project/-/merge_requests/17",
    "reviewer_user_id": 123,
    "reviewer_username": "orpheus"
  },
  "review": {
    "diff_fingerprint": "...",
    "review_fingerprint": "...",
    "artifacts_path": ".orpheus/reviews/<diff-fingerprint>",
    "trigger": "reviewer_assigned"
  },
  "diff_refs": {
    "base_sha": "...",
    "start_sha": "...",
    "head_sha": "..."
  },
  "protocol": {
    "artifact_schema_version": 1,
    "bundle_schema_version": 1,
    "helper_sha256": "..."
  }
}
```

Metadata не должна содержать token, raw credentials или данные, нужные агенту, но отсутствующие в
prompt/env. Connector считает metadata immutable contract и сверяет с ней HookResult.

### 7.6. Ограничение размера request

Instructions, hooks, helper bootstrap, rendered prompt и metadata входят в `CreateSession` request.
Connector проверяет локально настроенный безопасный предел до вызова API.

Если request слишком велик:

- ничего молча не обрезается;
- session не создаётся;
- connector публикует в MR краткую безопасную причину;
- ссылки на Web UI нет, потому что session ID не существует;
- connector пытается снять bot-reviewer;
- analysis retry не выполняется.

## 8. Подготовка AgentBox: `before_run`

`before_run` генерируется Go-кодом connector-а из embedded assets и immutable session contract. Для
one-shot session он выполняется один раз непосредственно перед агентом.

Основные шаги:

1. включить fail-fast shell mode;
2. проверить наличие требуемых CLI/runtime;
3. установить helper из встроенного compressed/base64 payload в content-addressed path;
4. проверить SHA-256 установленных bytes;
5. создать review directory и все ожидаемые subdirectories;
6. записать connector-owned contract manifest, необходимый helper-у;
7. клонировать repository, если workspace ещё не содержит checkout;
8. получить `refs/merge-requests/<iid>/head`;
9. выполнить detached checkout ровно на pinned `head_sha`;
10. проверить разрешимость `base_sha` и `head_sha`;
11. не оставлять в stdout произвольный protocol output.

Пример layout:

```text
<workspace>/
  .git/
  .orpheus/
    gitlab-review/
      code/<helper-sha>/review-pack
    reviews/<diff-fingerprint>/
      contract.json
      findings/
      confirmed/
      rejected/
      recommendations/
      resolutions/
      tmp/
```

`tmp/` разрешён только для временной работы review/compliance tools. Он не входит в publication
bundle. Helper находится по content-addressed path. `after_run` снова сверяет его hash, поэтому
случайное или агентное изменение helper-а превращается в явную ошибку вместо исполнения
подменённого кода.

Доставка helper-а не требует отдельного image/template release: bytes живут в connector binary и
передаются как часть hook script по тому же принципу, что файловый helper в Mattermost connector.
Предпочтительна реализация без сторонних runtime dependencies; если используется Python, разрешена
только стандартная библиотека и наличие Python становится явным требованием sandbox template.

## 9. Работа агента и файловый контракт

### 9.1. Pinned diff

Агент всегда начинает с эквивалента:

```sh
git diff "$ORPHEUS_GITLAB_DIFF_BASE_SHA"..."$ORPHEUS_GITLAB_DIFF_HEAD_SHA"
git log "$ORPHEUS_GITLAB_DIFF_BASE_SHA".."$ORPHEUS_GITLAB_DIFF_HEAD_SHA" --oneline
```

Он не переключает checkout и не расширяет scope до более нового branch state. Неизменённые файлы
можно читать как контекст, но findings создаются только для нарушений, внесённых pinned diff или
ставших применимыми из-за него.

### 9.2. Каталоги

```text
.orpheus/reviews/<diff-fingerprint>/
  findings/          незавершённые кандидаты; перед успехом должен быть пуст
  confirmed/         перепроверенные findings для публикации
  rejected/          отклонённые кандидаты; остаются только внутри VM
  recommendations/   рекомендации по правилам проекта
  resolutions/       намерения закрыть собственные исправленные discussions
```

Агент не читает artifacts другого diff fingerprint.

### 9.3. Finding

Один finding хранится в отдельном Markdown-файле с YAML front matter:

```markdown
---
id: F-0001
path: internal/service.go
line: 42
severity: warning
title: Неполная проверка
source: AGENTS.md
---

Самодостаточное объяснение проблемы, доказательства, выполненные проверки и рекомендуемое исправление.
```

Обязательные проверки helper-а:

- имя файла и `id` согласованы и уникальны;
- `path` repo-relative, clean, не абсолютный и не выходит через `..`;
- `line` — положительное целое;
- `severity` принадлежит `info`, `warning`, `error`;
- `title`, `source` и body непусты;
- размер каждого поля и число findings находятся в пределах;
- дубликаты IDs и семантически неоднозначные файлы запрещены;
- неизвестные обязательные поля/версии обрабатываются fail closed.

Helper проверяет структурную корректность. Принадлежность path/line текущему diff и построение GitLab
position повторно проверяет connector по свежему diff.

### 9.4. Жизненный цикл finding

Новый кандидат немедленно появляется в `findings/`. После отдельной перепроверки агент атомарно
перемещает его:

- в `confirmed/`, если проблема доказана;
- в `rejected/`, если вывод неверен; body дополняется причиной отклонения.

Успешный bundle возможен только при пустом `findings/`. В отличие от metal, оставшийся candidate не
планирует новый analysis attempt: helper завершает `after_run` ошибкой, а workflow становится
терминально ошибочным.

### 9.5. Recommendations

Recommendation — непустой Markdown-файл с наблюдаемым проектным правилом, связанными finding IDs и
предлагаемым местом документации. Recommendation не является inline finding и публикуется обычной
MR note с отдельным idempotency marker.

### 9.6. Resolution intents

Resolution intent описывает только собственную discussion bot-а, для которой агент доказал, что
исходная причина исправлена в pinned diff:

```markdown
---
discussion_id: discussion-id
note_id: 123
marker: "<!-- orpheus-review-finding:... -->"
---

Проверенный path:line, причина исходного замечания, выполненная проверка и доказательство исправления.
```

Connector перед mutation заново проверяет:

- note принадлежит immutable reviewer user ID;
- discussion ещё не resolved;
- note resolvable;
- trailing marker корректен и принадлежит Orpheus finding;
- `discussion_id`/`note_id` совпадают;
- diff всё ещё текущий.

Чужие discussions никогда не закрываются.

## 10. `after_run`, helper и publication bundle

### 10.1. Независимость от поведения агента

Агент не формирует JSON и не выбирает поля transport protocol. `after_run` всегда вызывает
сгенерированный connector-ом script, который:

1. проверяет bytes/hash helper-а;
2. передаёт helper-у только путь текущего review directory;
3. ожидает ровно один JSON envelope в stdout;
4. переносит exit code helper-а в hook result.

Все identity-данные helper читает из connector-owned `contract.json` внутри переданного каталога.
Даже если агент изменил этот файл, connector после получения bundle сверяет значения с immutable
message metadata. Несовпадение делает bundle неприемлемым.

### 10.2. Что входит в bundle

В bundle попадает только то, что требуется для будущей публикации после удаления VM:

- полный текст и metadata всех `confirmed` findings;
- полный текст recommendations;
- полный текст и target metadata resolution intents;
- identity, fingerprints, workflow/helper/schema versions;
- counts и, при необходимости, hashes для аудита.

Не входят:

- содержимое `rejected/`;
- временные файлы;
- repository checkout;
- command output и произвольные logs;
- незавершённые `findings/`;
- секреты.

Rejected artifacts валидируются локально настолько, насколько это нужно для проверки полного
жизненного цикла, но за пределы VM достаточно передать `rejected_count` и опциональный digest без
текста.

### 10.3. Логический payload

Ниже приведён ориентир schema, а не требование к точному Go naming:

```json
{
  "schema_version": 1,
  "stage": "ready",
  "workflow": {
    "id": "gitlab-mr-review",
    "revision": "sha256:...",
    "helper_sha256": "..."
  },
  "identity": {
    "gitlab_host": "https://gitlab.example.com",
    "project_id": 42,
    "mr_iid": 17,
    "reviewer_user_id": 123
  },
  "review": {
    "diff_fingerprint": "...",
    "review_fingerprint": "..."
  },
  "counts": {
    "confirmed": 1,
    "rejected": 2,
    "recommendations": 0,
    "resolutions": 1,
    "pending": 0
  },
  "confirmed": [
    {
      "id": "F-0001",
      "path": "internal/service.go",
      "line": 42,
      "severity": "warning",
      "title": "Неполная проверка",
      "source": "AGENTS.md",
      "body": "..."
    }
  ],
  "recommendations": [],
  "resolutions": []
}
```

Payload строится детерминированно: фиксированная JSON schema, стабильный порядок arrays, UTF-8,
нормализация всех переводов строк в LF (`\n`) и отсутствие map-order dependence. Это делает hashes
и idempotency markers воспроизводимыми.

### 10.4. Transport envelope

Payload сериализуется, сжимается zlib и кодируется base64. `after_run` печатает компактный envelope:

```json
{
  "schema_version": 1,
  "encoding": "zlib+base64",
  "payload_sha256": "...",
  "uncompressed_bytes": 12345,
  "payload": "eN..."
}
```

Connector проверяет envelope в следующем порядке:

1. JSON синтаксически корректен и не содержит trailing data;
2. версия и encoding поддерживаются;
3. declared sizes находятся в пределах до allocation/decompression;
4. base64 корректен;
5. распаковка не превышает uncompressed limit;
6. фактический размер совпадает с `uncompressed_bytes`;
7. SHA-256 несжатого payload совпадает;
8. payload schema валидна;
9. counts согласованы с arrays;
10. stage равен `ready`, pending равен нулю;
11. identity, fingerprints, revision и helper hash согласованы с metadata.

### 10.5. Размеры

Orpheus по умолчанию ограничивает hook output 512 KiB. Connector/helper используют меньший предел,
например 480 KiB для полного stdout envelope, оставляя запас на transport implementation. Точное
значение конфигурируемо только в безопасных пределах и не должно превышать server contract.

Дополнительно действует предел распакованного payload, например 4 MiB, и per-item/count limits.
Это защищает connector от zip bomb и неконтролируемого memory use.

Если bundle не помещается:

- helper завершается с явной категорией `review_bundle_too_large`;
- данные не обрезаются;
- частичный bundle не публикуется;
- S3, GitLab uploads и другие fallback transports не используются;
- workflow заканчивается ошибкой без analysis retry.

## 11. Условия приёма HookResult

Наличие JSON в `after_run.output` само по себе недостаточно. Connector принимает bundle только если:

- найден ровно ожидаемый run для session;
- run имеет terminal status `completed`;
- agent phase завершилась успешно, а не failed/cancelled;
- `after_run` присутствует;
- hook status равен `completed`;
- hook exit code равен нулю;
- `output_completeness` равен `complete`;
- `truncation_reason` отсутствует;
- output имеет ожидаемый text result, а не unavailable/truncated variant;
- envelope и payload прошли все проверки раздела 10;
- metadata schema и bundle schema совместимы с текущим connector-ом.

`output_completeness=truncated`, `unavailable` или `unknown` всегда означает workflow error. Connector
не пытается «дочитать» output из VM и не публикует частичный результат.

`after_run` может быть записан Orpheus и после неуспешного agent phase, но такой bundle не
публикуется: успешное завершение агента является отдельным обязательным условием.

Интерпретация статусов Orpheus:

| Run status | Действие connector-а |
|---|---|
| `accepted`, `starting`, `running` | считать run активным и продолжать GitLab reconciliation |
| `cancelling` | не публиковать, дожидаться terminal status без повторного анализа |
| `finalizing` | не читать промежуточный HookResult; `after_run` ещё может выполняться |
| `completed` | проверить agent phase, after_run и bundle по полному acceptance contract |
| `failed` | workflow error lifecycle |
| `cancelled` | применить причину отмены; поздний output не публиковать |

## 12. Publication pipeline

### 12.1. Общие инварианты

Только connector выполняет GitLab mutations. Перед каждой mutation он повторно получает MR и
сверяет текущий `diff_fingerprint` с bundle. Проверка один раз в начале недостаточна: diff может
измениться между двумя HTTP calls.

Публикация идемпотентна. Каждая создаваемая note/discussion заканчивается скрытым marker. Marker
считается существующим только если:

- он является точным trailing marker;
- note создана immutable reviewer user ID;
- marker format и hash валидны.

Текст, похожий на marker и написанный другим пользователем, не создаёт ownership.

### 12.2. Порядок публикации

После приёма bundle connector:

1. загружает свежие MR, diffs и discussions;
2. проверяет `opened`, reviewer assignment и diff fingerprint;
3. для каждого confirmed finding проверяет marker;
4. строит GitLab diff position для `path:line` новой версии;
5. создаёт inline discussion либо разрешённый fallback note;
6. публикует recommendations с отдельными markers;
7. применяет допустимые resolution intents;
8. убеждается, что нет необработанной permanent/transient ошибки;
9. снова проверяет diff;
10. публикует completion note с числом подтверждённых замечаний;
11. проверяет наличие completion marker при uncertain response;
12. снимает только bot-reviewer, не меняя human reviewers.

Completion marker является границей полностью опубликованного результата. Он не создаётся, пока
какой-либо обязательный item не опубликован или не распознан как уже опубликованный.

### 12.3. Markers

Сохраняется семантика metal markers:

```text
<!-- orpheus-review-finding:<hash> -->
<!-- orpheus-review-recommendation:<hash> -->
<!-- orpheus-review-complete:<review-fingerprint> -->
<!-- orpheus-review-skipped:<review-fingerprint> -->
<!-- orpheus-review-error:<hash> -->
```

Finding hash строится из versioned normalized набора как минимум:

- review/diff fingerprint;
- finding ID;
- normalized path;
- line;
- severity;
- title;
- body;
- source.

Формат marker-а версионируется, если меняется нормализация. Старые поддерживаемые markers должны
распознаваться при reconciliation in-flight reviews.

### 12.4. Inline fallback

Если connector доказал, что inline position невалидна — path отсутствует в diff, line нельзя
однозначно сопоставить hunk или GitLab вернул подтверждённую invalid-position ошибку — finding
публикуется обычной MR note. Note содержит исходный finding, ожидаемый `path:line`, краткую причину
fallback и тот же idempotency marker.

Fallback не применяется к authorization, validation или неизвестной 4xx ошибке. Нельзя маскировать
ошибку API как invalid position.

### 12.5. Completion

При нуле замечаний completion note сообщает, что текущая версия MR проверена и замечаний не найдено.
При наличии замечаний указывается их число. В обоих случаях явно говорится, что автоматическое
ревью не является approve и не заменяет человеческую проверку.

Reviewer снимается только после подтверждённого completion marker. Если note уже создана, но response
потерян, connector сначала читает discussions/notes и проверяет marker, а не создаёт дубликат.

## 13. Ошибки и retries

### 13.1. Главное правило

Analysis retry отсутствует. Ошибка подготовки, агента, VM, harness, helper-а или bundle validation
никогда не запускает новую session автоматически. Одна reviewer assignment создаёт не более одного
Codex run.

Допускаются lifecycle retries, которые не повторяют анализ:

- повтор idempotent `CreateSession` после неопределённого HTTP результата;
- чтение состояния Orpheus;
- retry transient GitLab fetch/mutation;
- проверка marker после uncertain mutation;
- повтор краткой error note;
- повтор удаления reviewer;
- повтор cancel request.

### 13.2. Workflow errors

К workflow errors относятся, в частности:

- request/prompt слишком велик до создания session;
- `before_run` не смог установить helper или подготовить checkout;
- AgentBox/worker/harness завершился ошибкой;
- run timeout/stall;
- agent phase failed;
- `findings/` не пуст;
- artifact schema нарушена;
- helper hash не совпал;
- after_run failed/skipped/cancelled;
- HookResult truncated/unavailable/unknown;
- bundle слишком велик;
- bundle не согласован с immutable metadata;
- schema version не поддерживается.

Для workflow error connector по возможности:

1. определяет безопасную краткую причину;
2. публикует одну error note с hidden idempotency marker;
3. добавляет ссылку на Orpheus Web UI, если session существует;
4. снимает bot-reviewer;
5. не запускает повторный анализ.

Видимый текст содержит только причину и ссылку. Run/session IDs, stack traces, raw hook output,
bundle, tokens и внутренние адреса не включаются. Подробности остаются в Orpheus и structured logs.

Если session не была создана, ссылка отсутствует, а note содержит только причину.

### 13.3. Publication errors

Ошибки GitLab publication классифицируются:

| Категория | Поведение |
|---|---|
| network timeout, connection error, HTTP 429, HTTP 5xx | lifecycle retry с backoff; при mutation сначала marker verification |
| response потерян после mutation | refetch и проверка marker, затем retry только при доказанном отсутствии |
| подтверждённая invalid inline position | ordinary-note fallback |
| прочие HTTP 400/401/403/404 | terminal publication error |
| stale diff | прекратить публикацию, cancel/ignore bundle, снять reviewer по stale lifecycle |

При terminal publication error уже опубликованные items не удаляются. Completion marker не ставится.
Connector пытается опубликовать error note, если permissions это позволяют, и снять reviewer.

Transient publication retry может продолжаться согласно настроенному backoff/timeout policy, пока MR
не перешёл в состояние, отменяющее операцию. Он использует уже сохранённый HookResult и никогда не
поднимает новую VM.

### 13.4. Новое ручное назначение после ошибки

Если человек снова назначает bot-reviewer после error lifecycle, GitLab создаёт новое значимое
assignment activity. Новый `review_fingerprint` позволяет создать новую независимую session. Это
единственный путь повторить анализ: явное новое пользовательское действие.

## 14. Отмена активного ревью

Connector продолжает polling GitLab во время активного Orpheus run.

| Наблюдаемое событие | Действие с run | GitLab mutations |
|---|---|---|
| MR closed или merged | `CancelRun`, bundle игнорируется | не писать notes, reviewer не трогать |
| reviewer удалён человеком | `CancelRun`, bundle игнорируется | ничего не публиковать и не возвращать reviewer |
| diff fingerprint изменился | `CancelRun`, bundle игнорируется | снять bot-reviewer; новый diff требует ручного назначения |
| diff refs стали неполными | `CancelRun`, bundle игнорируется | снять bot-reviewer |
| временно не удалось fetch MR | run не отменять | повторить polling позже |
| connector перезапускается/deploy | run не отменять | новая версия продолжит reconciliation |
| timeout/stall/VM/harness failure | terminal workflow error | краткая error note и удаление reviewer |

Cancel является best effort и идемпотентен. После решения об отмене никакой поздний успешный
HookResult этой session не может быть опубликован. Перед publication connector всё равно повторяет
проверки, поэтому race «агент закончил одновременно с изменением diff» закрывается свежим GitLab
state.

Изменение workflow revision не является событием отмены.

## 15. Durable state и восстановление

### 15.1. Отсутствие собственной БД

Connector не хранит durable state локально. Источники истины:

| Данные | Durable source |
|---|---|
| session/run/status/hooks/history | Orpheus PostgreSQL через API |
| immutable integration contract | metadata первого user message |
| publication payload | complete `after_run` HookResult |
| факт опубликованного item/completion/error | GitLab note/discussion marker от bot user ID |
| актуальность MR и reviewer | GitLab MR |
| временные очереди/backoff | память connector-а; после рестарта вычисляются заново |

AgentBox filesystem не является durable state connector-а. После terminal run он может исчезнуть.

### 15.2. Восстановление после рестарта

После запуска connector:

1. валидирует config и authenticated GitLab reviewer identity;
2. получает текущих GitLab candidates;
3. для каждого MR ищет Orpheus sessions/runs по namespace, external key и input fingerprint;
4. читает metadata и проверяет schema/ownership;
5. связывает active run с текущим MR;
6. для terminal successful run читает HookResult и продолжает publication;
7. для terminal failed run продолжает error lifecycle;
8. для stale/removed/closed MR применяет cancellation matrix;
9. проверяет GitLab markers перед каждой повторяемой mutation;
10. только после reconciliation допускает новый review.

Если connector упал:

- после успешного CreateSession, но до получения response — стабильный Idempotency-Key возвращает тот
  же acceptance;
- после finding note, но до ответа GitLab — marker verification обнаруживает note;
- после completion note, но до удаления reviewer — completion marker приводит только к повтору
  удаления reviewer;
- после error note, но до удаления reviewer — error marker приводит только к повтору удаления;
- во время активного run — worker продолжает работу независимо, новая версия connector-а подхватывает
  session;
- после получения bundle, но до publication — bundle остаётся в Orpheus PostgreSQL.

Полное сканирование бесконечной истории Orpheus не требуется: GitLab reviewer assignment задаёт
рабочий набор, а namespace/external key/fingerprint позволяют адресно найти связанную session.
Реализация может дополнительно использовать bounded lookback, но он не должен создавать окно потери
для MR, где bot всё ещё назначен.

### 15.3. Конкурентность

Текущая архитектура поддерживает ровно одну активную реплику connector-а на один GitLab source и
reviewer identity. Это явное эксплуатационное ограничение, а не требование Kubernetes.

Причина: без distributed lock две реплики могут одновременно принять одного кандидата. Orpheus
idempotency и GitLab markers уменьшают последствия, но не заменяют формальный distributed admission
lock для всех race conditions.

При Kubernetes deployment используется одна replica и стратегия последовательной замены. До снятия
ограничения нельзя включать rolling overlap двух ready replicas.

## 16. Graceful shutdown и deployment

При SIGTERM/process shutdown connector:

1. немедленно переводит readiness в false;
2. прекращает новые polling/admission cycles;
3. не вызывает `CancelRun` для активных Orpheus runs;
4. не ждёт завершения Codex или AgentBox;
5. даёт только уже начатым внешним HTTP calls закончиться в короткий bounded timeout;
6. завершает процесс.

Новая версия после старта восстанавливает sessions из Orpheus и publication state из GitLab markers.
Благодаря этому recreate не обрывает дорогой анализ и не требует держать старый pod до окончания VM.

Для безопасного deployment новой версии:

- новая версия должна читать предыдущую persistable metadata/bundle schema;
- старые in-flight sessions завершаются со своей embedded workflow revision;
- новая версия не пересоздаёт их с новыми instructions/helper;
- readiness старой реплики должна быть снята до admission новой, пока действует single-replica
  constraint.

## 17. Безопасность и trust boundaries

### 17.1. Trust model

Доверенными считаются:

- connector binary и его embedded assets;
- Orpheus API authentication и сохранённая metadata;
- helper bytes после hash verification;
- GitLab API response, полученный authenticated connector-ом.

Недоверенными считаются:

- MR title/description и repository contents;
- agent-generated files до helper validation;
- финальный текст агента;
- filenames/front matter/body artifacts;
- похожие на Orpheus markers notes других пользователей;
- compressed bundle до полного bounded validation.

### 17.2. GitLab mutations из VM

Целевая граница требует, чтобы GitLab mutations выполнял только connector. Однако на первом этапе
агенту может быть доступен credential и `glab` для read-only проверок, а запрет mutations задаётся
только developer instructions.

Это **не является технической security boundary**: ошибившийся или скомпрометированный агент
теоретически может выполнить mutation с доступным token.

Явная зависимость до 1.0:

- разделить connector и sandbox credentials/scopes либо предоставить harness-level read-only proxy;
- технически исключить mutation-capable GitLab credential из agent environment;
- сохранить connector единственным mutation principal.

До реализации этого требования документация и threat model не должны утверждать, что запрет
гарантируется платформой.

### 17.3. Защита publication protocol

- Helper проверяется SHA-256 непосредственно перед запуском.
- Bundle сверяется с immutable metadata, а не только с workspace contract.
- Decompression bounded по входу и выходу.
- Paths очищаются и не могут выходить из repository.
- GitLab marker признаётся собственным только по author ID и exact trailing form.
- Diff сверяется перед каждой mutation.
- Raw bundle и hook output не логируются.
- Error note не раскрывает внутреннюю диагностику.

## 18. GitLab adapter

### 18.1. Начальная реализация

Для быстрого переноса поведения разрешено адаптировать существующий metal adapter, который вызывает
`glab` subprocess. На этом этапе `glab` является явной runtime dependency образа connector-а.

Adapter должен предоставлять типизированные операции:

- current user;
- list candidates;
- fetch MR/project/notes/diffs/discussions;
- create MR note;
- create inline discussion;
- resolve discussion;
- remove reviewer.

Остальной код не должен зависеть от stdout format и subprocess details: они инкапсулируются внутри
adapter-а и покрываются contract tests.

### 18.2. Обязательное условие до 1.0

До версии 1.0 `glab` subprocess adapter **ДОЛЖЕН** быть заменён нативным Go GitLab client. После
миграции:

- `glab` удаляется из runtime dependencies connector-а;
- HTTP status и GitLab error payload классифицируются без разбора CLI text;
- context cancellation, timeouts и rate-limit handling становятся явными;
- uncertain mutation и invalid-position errors получают типизированное представление;
- API compatibility проверяется integration/contract tests.

Это обязательная зависимость release 1.0, а не необязательный refactoring.

## 19. Observability

### 19.1. Structured logs

Каждое событие содержит безопасные correlation fields:

- workflow ID/revision;
- normalized GitLab host label без credentials;
- project ID и MR IID;
- reviewer user ID;
- session ID и run ID, если существуют;
- diff/review fingerprint в сокращённой или полной безопасной форме;
- lifecycle phase;
- error category;
- retry attempt внешней операции.

Не логируются tokens, clone credentials, полный prompt, raw artifact bodies, полный bundle или hook
stdout.

### 19.2. Метрики

Рекомендуемый минимальный набор:

- poll duration/errors и число candidates;
- active sessions по Orpheus status;
- admissions и suppressed duplicates;
- run duration и terminal outcomes;
- cancellations по причине;
- bundle compressed/uncompressed size;
- artifact counts;
- publication items по типу и outcome;
- transient/permanent GitLab errors;
- lifecycle retry count/age;
- reviewer removal latency;
- reconciliation/recovery count;
- readiness и время graceful shutdown.

Labels не должны содержать project path/MR IID в системах, где это создаст неконтролируемую
cardinality; такие поля остаются в logs/traces.

### 19.3. Диагностика пользователем

Пользователь видит в MR только итоговые findings/recommendations/completion либо краткую error note.
Подробная причина доступна по ссылке на Orpheus Web UI. Connector logs связываются с UI через
session/run IDs.

## 20. Состояния и переходы

Логическая state machine одного ревью:

```text
OBSERVED
  ├─ ineligible/temporary fetch error ───────────────→ OBSERVED
  ├─ already completed/handled ─────────────────────→ RECONCILED
  └─ eligible + capacity
        ↓
CREATING_SESSION
  ├─ uncertain request ── idempotent lookup/retry ──┐
  ├─ permanent preparation error ───────────────────→ ERROR_LIFECYCLE
  └─ accepted                                        │
        ↓                                            │
RUNNING ←────────────────────────────────────────────┘
  ├─ closed/reviewer removed ───────────────────────→ CANCELLED_SILENT
  ├─ diff changed/refs incomplete ──────────────────→ STALE_LIFECYCLE
  ├─ run/agent/harness failed ──────────────────────→ ERROR_LIFECYCLE
  └─ run completed + valid HookResult
        ↓
READY_TO_PUBLISH
  ├─ current diff changed ──────────────────────────→ STALE_LIFECYCLE
  ├─ transient GitLab error ────────────────────────→ READY_TO_PUBLISH
  ├─ permanent publication error ───────────────────→ ERROR_LIFECYCLE
  └─ all items + completion marker
        ↓
REMOVING_REVIEWER
  ├─ transient error ───────────────────────────────→ REMOVING_REVIEWER
  └─ reviewer absent
        ↓
COMPLETED

ERROR_LIFECYCLE
  ├─ ensure error marker
  ├─ remove reviewer
  └─────────────────────────────────────────────────→ FAILED

STALE_LIFECYCLE
  ├─ cancel/ignore late result
  ├─ remove reviewer
  └─────────────────────────────────────────────────→ STALE
```

Эти состояния не обязаны храниться отдельной таблицей. Они выводятся из GitLab state/markers и
Orpheus run/hooks/metadata.

## 21. Основные алгоритмы

### 21.1. Poll/reconcile loop

```text
validate/reload immutable process config at startup only
resolve authenticated reviewer ID

every poll interval:
    fetch candidate MRs
    for each candidate in stable order:
        fetch consistent MR input
        compute MR key, diff fingerprint, review fingerprint
        find related Orpheus session(s)
        reconcile active/terminal session and GitLab markers
        if lifecycle work remains:
            perform/buffer lifecycle work
            continue
        if already handled:
            continue
        if capacity unavailable:
            leave candidate for later poll
            continue
        render workflow request
        create session with stable idempotency key

    reconcile active sessions whose MR disappeared from candidate list:
        fetch MR directly
        apply cancellation matrix
```

Последний шаг нужен, потому что reviewer removal и MR closure сами убирают объект из обычного списка
кандидатов, но active run всё ещё необходимо отменить.

### 21.2. Bundle reconciliation

```text
load immutable message metadata
load run and phase/hook results

if run is non-terminal:
    compare current MR with metadata and possibly cancel
    return

if run is cancelled because of domain cancellation:
    ignore any output
    finish corresponding lifecycle
    return

if run/agent/after_run is not fully successful:
    ensure error note marker
    ensure reviewer removed
    return

decode and validate bundle with bounded resources
compare bundle contract to immutable metadata
publish idempotently, checking current diff before every mutation
ensure completion marker
ensure reviewer removed
```

### 21.3. Idempotent mutation

```text
fetch own notes/discussions
if exact marker already exists:
    treat operation as complete
    return

check current diff and lifecycle preconditions
attempt mutation
if result is certain success:
    return
if result is uncertain/transient:
    refetch own notes/discussions
    if marker exists:
        return
    schedule lifecycle retry with backoff
if result is permanent:
    enter terminal publication error
```

## 22. Тестовая стратегия

### 22.1. Unit tests

- host/path normalization;
- diff/review fingerprint golden tests;
- significant activity filtering;
- workflow revision hashing;
- strict template rendering and escaping;
- request-size accounting;
- artifact front matter parsing;
- path traversal and invalid UTF-8 rejection;
- deterministic helper output;
- envelope compression/hash/size validation;
- decompression bomb protection;
- schema compatibility;
- marker generation/recognition and author ownership;
- GitLab error classification;
- cancellation decision table;
- graceful shutdown admission behavior.

### 22.2. Contract tests с Orpheus

- atomic `CreateSession` и Idempotency-Key replay;
- `allow_multiple_runs=false` действительно запрещает следующий run;
- metadata сохраняется и не попадает агенту;
- hook env доступен только согласованным hooks;
- after_run result сохраняется после terminal run;
- complete/truncated/unavailable outputs различаются;
- cancel races и terminal statuses;
- поиск sessions/runs по namespace/external key/input fingerprint;
- compatibility с предыдущей schema version.

### 22.3. GitLab adapter tests

- pagination кандидатов, notes, diffs и discussions;
- reviewer identity и project filtering;
- inline position для added/changed hunks;
- invalid-position classification;
- marker verification после lost response;
- сохранение human reviewers при удалении bot-а;
- отказ закрывать чужую discussion;
- rate limit и server error retry;
- permanent auth/not-found handling.

### 22.4. End-to-end scenarios

1. clean review без findings;
2. несколько inline findings и completion;
3. invalid inline position с note fallback;
4. recommendation и valid resolution intent;
5. agent оставил pending finding;
6. helper подменён или bundle повреждён;
7. bundle превышает 480 KiB;
8. diff изменился во время анализа;
9. diff изменился между двумя publication items;
10. reviewer удалён человеком во время run;
11. MR закрыт во время run;
12. connector рестартован во время run;
13. connector рестартован после finding, completion или error note;
14. CreateSession принят, но HTTP response потерян;
15. GitLab mutation выполнена, но response потерян;
16. workflow revision обновлена при активной старой session;
17. новое ручное назначение после failed review;
18. две реплики ошибочно запущены — тест должен демонстрировать/детектировать unsupported topology.

## 23. Совместимость и версионирование

Независимо версионируются:

- message metadata schema;
- artifact file schema;
- bundle payload schema;
- transport envelope schema;
- marker normalization/version;
- workflow revision.

Connector должен fail closed на неизвестной major/schema version: не публиковать данные, а завершить
workflow error lifecycle. Добавление необязательного backward-compatible поля не обязано менять
schema version, но semantic assets всё равно меняют workflow revision.

Минимальная политика deployment: новая версия поддерживает все schema versions, которые могла
создать непосредственно предыдущая версия и которые ещё могут находиться в пределах максимального
run/publication lifetime. Удаление reader-а старой schema требует доказательства отсутствия таких
in-flight sessions.

## 24. Эксплуатационные инварианты

Следующие утверждения должны оставаться истинными при любой реализации:

1. Один review assignment не запускает больше одного model analysis.
2. Один analysis выполняется в одной non-reusable VM.
3. Изменение workflow revision не вмешивается в уже созданную session.
4. Ни один finding не публикуется из финального ответа агента.
5. Ни один непроверенный/частичный/truncated bundle не публикуется.
6. Перед каждой GitLab mutation проверяется актуальность diff.
7. Connector никогда не закрывает discussion другого автора.
8. Human reviewers не удаляются.
9. Рестарт connector-а не отменяет active Orpheus run.
10. Отсутствие собственной БД не приводит к потере результата: durable evidence находится в
    Orpheus и GitLab.
11. Lifecycle retry не создаёт новую VM и не повторяет модель.
12. Artifact files и repository checkout не покидают AgentBox; наружу выходит только ограниченный
    publication bundle.
13. Ошибка видна в MR и диагностируема через Orpheus Web UI, если session существует.
14. Секреты не входят в prompt, metadata, bundle, markers и logs.
15. Пока нет distributed lock, одновременно работает одна replica connector-а.

## 25. Принятые ограничения и работа до 1.0

### 25.1. Текущие ограничения

- Только polling; webhooks отсутствуют.
- Только одна активная replica на GitLab source/reviewer.
- Workflow один и встроен в бинарник; hot reload отсутствует.
- Session строго one-shot; resume/reuse не поддерживаются.
- Analysis retry отсутствует.
- Publication bundle ограничен HookResult limit Orpheus.
- На первом этапе GitLab adapter может зависеть от `glab`.
- На первом этапе запрет agent-side GitLab mutations обеспечивается инструкциями, не отдельным
  read-only credential boundary.

### 25.2. Обязательные задачи до 1.0

1. Заменить `glab` adapter нативным Go GitLab client.
2. Технически исключить GitLab mutation capability из AgentBox/agent credential path.
3. Зафиксировать и протестировать versioned metadata/artifact/bundle/marker schemas.
4. Доказать restart recovery для каждой точки неопределённости внешнего вызова.
5. Документировать single-replica deployment и сделать overlapping readiness невозможной либо
   очевидно ошибочной.
6. Проверить совместимость connector release с in-flight sessions предыдущего release.

Поддержка нескольких активных replicas потребует отдельного решения: distributed lease/lock либо
атомарный admission primitive с доказанной fencing semantics. Само наличие Orpheus idempotency key
не снимает эту задачу.

## 26. Итоговая модель

`orpheus-gitlab-mr-review` — не место исполнения агента и не файловый transport. Это
GitLab-specific connector/reconciler, который превращает reviewer assignment в неизменяемую
одноразовую задачу Orpheus и затем превращает проверенный HookResult в идемпотентные GitLab
mutations.

Orpheus отвечает за durable execution state и изолированную VM. Agent отвечает только за анализ и
локальные artifacts. Embedded helper создаёт строгую границу между недоверенными файлами агента и
versioned publication protocol. GitLab markers и Orpheus metadata/HookResult вместе заменяют
собственную БД connector-а.

Критическая последовательность всегда остаётся такой:

```text
назначение reviewer
  → frozen input snapshot
  → atomic one-shot Orpheus session
  → pinned analysis в новой AgentBox VM
  → deterministic validation/packing в after_run
  → durable HookResult в Orpheus PostgreSQL
  → fresh diff verification
  → idempotent publication в GitLab
  → completion/error marker
  → удаление bot-reviewer
```

Любое отклонение от этой последовательности должно сохранять два свойства: устаревший результат не
публикуется, а неопределённость внешнего вызова не приводит к повторному запуску анализа.
