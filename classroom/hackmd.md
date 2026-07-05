
---
title: 'OpenTelemetry Operator #1'
tags: [' OpenTelemetry']
---
前言
 企業內部動輒幾十、上百個服務，要達成全鏈路追蹤，傳統做法是：每個團隊自己在程式碼裡接 SDK、自己在部署裡掛一個 sidecar collector。這條路走起來大概是這樣——

- 平台團隊寫好接入文件，然後追著每個 repo 求人裝 SDK；裝不裝、什麼時候裝，取決於各團隊的排期。
- 就算都裝了，每個服務的 agent 版本、一些設定、上報 endpoint 各自為政。想統一升級一次 Java agent？50 個 repo、50 個 PR。
- Collector 的設定散落在各團隊的 Helm values 和 manifest 裡。想全公司調一次 sampling 政策或遮罩規則，等於要動所有人的部署。

換句話說，這裡有兩層問題：第一層是「接不上」——靠人手動接入，覆蓋率永遠是殘缺的；第二層是「管不動」——就算接上了，SDK 版本、collector 設定、資料政策沒有任何集中治理的手段，全靠文件和自律。

如果「接入」能做到不改一行程式碼、加個 annotation 就自動插樁，第一層問題就解了——這就是 OpenTelemetry Operator 的 auto-instrumentation。而它背後的 CR 和 OpAMP，正是第二層「怎麼治理這群 collector（設定、版本、政策）」的答案。

先從第一層看起。下面這張圖是 auto-instrumentation 的完整流程：。

如果能作到無需編寫程式碼即可自動插樁，那會極具吸引力。下一步的重點就是怎麼治理這些 Collector（設定/版本...）

![dice-app-diagram](https://hackmd.io/_uploads/rJYVsrImGx.jpg)

這張圖說明的是 OpenTelemetry Operator 的 auto-instrumentation（自動注入儀器化）完整流程，從應用部署到遙測資料進入 Grafana Cloud。以下依圖中編號解釋：

三個步驟

(1) 使用者部署 dice-app，並在 manifest 上宣告儀器化意圖

使用者不需要改任何程式碼，只在 Deployment 的 Pod template 加上 annotations：

```
spec:
  template:
    metadata:
      annotations:
        instrumentation.opentelemetry.io/inject-<<language>>: "true"   # 例如 inject-java
        resource.opentelemetry.io/service.name: "
        resource.opentelemetry.io/service.namespace: "game"
        resource.opentelemetry.io/service.version: "1.2.3"
        resource.opentelemetry.io/deployment.environment.name: "production"
```

- inject-<<language></language>> 告訴 Operator「幫這個 Pod 注入某語言的 instrumentation agent」
- resource.opentelemetry.io/* 系列則定義了遙測資料的 resource attributes（服務名、版本、環境等）

(2) Kubernetes 控制平面呼叫 OpenTelemetry Operator

當 Pod 要被建立時，API Server 會透過 mutating admission webhook 把 Pod spec 交給 Operator 處理。

(3) Operator 改寫（enrich）Pod spec

Operator 注入兩樣東西：

- OTEL_* 環境變數：例如 OTEL_SERVICE_NAME、OTEL_EXPORTER_OTLP_ENDPOINT、OTEL_RESOURCE_ATTRIBUTES，讓 instrumentation library 知道要送去哪、掛什麼標籤
- 一個容器來載入 instrumentation library：圖中稱為 sidecar，實務上是用 init container 把對應語言（Java、Node.js、Python、.NET、Go…）的 agent 複製進共用 volume，應用容器再透過環境變數（如 Java 的 JAVA_TOOL_OPTIONS）載入它

# Operator 四大 CR

## OpenTelemetryCollector — 資料管線的宣告書

角色：四個 CR 裡唯一 stable（v1beta1）的，也是核心。它宣告「我要一個什麼樣的 Collector」：跑在哪種形態（spec.mode：deployment / daemonset / statefulset / sidecar）、pipeline 長什麼樣（spec.config，就是 Collector 的 receivers/processors/exporters 設定）、幾個副本、多少資源。

功能：你寫一份 CR，Operator 的 Reconcile 迴圈幫你長出整組 Kubernetes 資源——Deployment/StatefulSet/DaemonSet、ConfigMap、Service、ServiceAccount，甚至根據 config 裡開了哪些 receiver 自動在 Service 上開對應的 port。sidecar mode 特殊：不會產生獨立 workload，而是變成一份範本，等 Pod 帶著 `sidecar.opentelemetry.io/inject` annotation 建立時，由 webhook 注入。

治理意義：這是資料政策的載體。sampling 比例、敏感資料遮罩（OTTL）、路由、資源上限，全部收斂在 CR 的 config 裡——一份可以進 Git、可以 review 的 YAML。而且不是「apply 一次就結束」：有人手改 Operator 管的 Deployment 或 ConfigMap，下一輪 Reconcile 就改回來，configuration drift 在機制層面被消滅。平台團隊擁有 gateway/agent 的 CR，等於擁有全公司 telemetry 的出口閘門。

```yaml
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: gateway
spec:
  mode: deployment        # 也可以是 daemonset / statefulset / sidecar
  replicas: 2
  config:                 # ← 就是原汁原味的 Collector 設定
    receivers:
      otlp:
        protocols:
          grpc: {}
          http: {}
    processors:
      memory_limiter:
        check_interval: 1s
        limit_percentage: 75
    exporters:
      otlphttp:
        endpoint: https://your-backend:4318
    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [memory_limiter]
          exporters: [otlphttp]
```

kubectl apply 之後，Operator 自動長出這些東西——你一個都不用自己寫：

```
$ kubectl get deploy,svc,cm -l app.kubernetes.io/instance=default.gateway
deployment.apps/gateway-collector            2/2
service/gateway-collector                    4317/TCP,4318/TCP   ← port 是從 config 裡的 receiver 推導的
service/gateway-collector-headless
service/gateway-collector-monitoring         8888/TCP
configmap/gateway-collector-<hash>
```

## Instrumentation — 全公司的儀器化規格書

角色：定義「自動插樁要怎麼做」：各語言（Java、Python、NodeJS、.NET、Go…）agent 的 image 與版本、上報的 exporter.endpoint、propagators、sampler、要注入哪些 env。

功能：它是四個 CR 裡最特別的——自己什麼都不建立，沒有對應的 Deployment，純粹是一份被 webhook 讀取的範本。業務團隊在 Pod 上加 `instrumentation.opentelemetry.io/inject-java: "true"`，Pod 建立當下 mutating webhook 依照這份 CR 注入 init container（把 agent 複製進共用 volume）和 OTEL_* 環境變數。應用 image 一個 byte 都不用改。

治理意義：這是接入標準的單一事實來源。50 個服務指向同一份 CR，升級全公司的 Java agent = 改 CR 裡一個 image tag，而不是追 50 個 repo 發 PR。因為它只是範本，可以放在平台的 namespace、用 RBAC 設成業務團隊唯讀——標準由平台定義，業務團隊只表達「我要接入」的意圖。要注意的邊界：webhook 只在 Pod CREATE 時生效，改 CR 對已經在跑的 Pod 無效，要等下一次 rollout。

平台團隊 apply 一次（全公司一份）：

```yaml=
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: company-default
  namespace: opentelemetry   # 建議放共用 namespace,pod 用 "ns/name" 跨 namespace 引用
spec:
  exporter:
    # 用 4318 (OTLP http/protobuf):Python 只支援 http/protobuf,
    # Java agent 2.x / NodeJS 預設也是 http/protobuf,統一 4318 最不會踩雷
    endpoint: http://gateway-collector:4318
  propagators:
    - tracecontext
    - baggage
  sampler:
    type: parentbased_traceidratio
    argument: "0.25"

  # 所有語言共用的 resource attributes
  # 注意:這裡是「最低優先級」,會被 k8s 自動屬性、Pod annotation、容器既有的
  # OTEL_RESOURCE_ATTRIBUTES 覆蓋(internal/instrumentation/sdk.go createResourceMap)。
  # service.namespace 不用在這裡設定——operator 預設自動取 Pod 所在的 k8s namespace,
  # 個別覆寫用 Pod annotation resource.opentelemetry.io/service.namespace。
  # 這裡只放真正全公司統一的靜態值。
  resource:
    resourceAttributes:
      deployment.environment.name: production
    # 是否額外加上 k8s.pod.uid / k8s.deployment.uid 等 UID 屬性。
    # 名稱屬性(k8s.deployment.name 等)不受影響、一律會加;
    # UID 是高基數屬性,每次 rollout 都換值,通常不開(false 也是預設值)
    addK8sUIDAttributes: false
  
  defaults:
    # 從 pod 的 app.kubernetes.io/name / version labels 自動帶出 service.name / service.version
    useLabelsForResourceAttributes: true
  
  # 所有語言共用的額外 env(可選)
  env:
    - name: OTEL_EXPORTER_OTLP_PROTOCOL
      value: http/protobuf
  
  # ---- 各語言:image 可省略(operator 會填預設版本);要鎖版就明確 pin ----
  java:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-java:2.28.1
    resources:
      requests: { cpu: 50m, memory: 64Mi }
      limits:   { cpu: 500m, memory: 64Mi }

  nodejs:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-nodejs:0.76.0

  python:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-python:0.63b1
    env:
      # Python 的 log 自動注入預設關閉,常會想開
      - name: OTEL_LOGS_EXPORTER
        value: otlp

  # Go 需要 operator 加 --enable-go-instrumentation=true,且是 eBPF sidecar(需要特權)
  go:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-go:v0.24.0

  apacheHttpd:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-apache-httpd:1.0.4

  # Nginx 需要 operator 加 --enable-nginx-instrumentation=true
  nginx:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-apache-httpd:1.0.4  # 與 httpd 共用同一個 image
```

業務團隊在自己的 Deployment 加一行：

```yaml
spec:
  template:
    metadata:
      annotations:
        # CR 放在共用 namespace 時，值要寫 "namespace/name" 跨 namespace 引用
        instrumentation.opentelemetry.io/inject-java: "opentelemetry/company-default"
```

annotation 的值有三種寫法，取決於 CR 放在哪：

| 值 | 意義 |
|---|---|
| `"true"` | 用 Pod 所在 namespace 裡唯一的一份 Instrumentation CR（有多份會失敗） |
| `"company-default"` | 用 Pod 所在 namespace 裡指定名字的 CR |
| `"opentelemetry/company-default"` | 跨 namespace 引用平台團隊統一維護的 CR（推薦） |

各語言的 annotation key 對應：`inject-java`、`inject-nodejs`、`inject-python`、`inject-dotnet`、`inject-go`、`inject-apache-httpd`、`inject-nginx`，以及一個特殊的 `inject-sdk`（只注入環境變數、不裝 agent，後面 PHP 一節會用到）。這些常數定義在 `internal/instrumentation/annotation.go`。

就這樣。Pod 重建後 kubectl describe pod 會看到多出來的東西：

```yaml=
Init Containers:
  opentelemetry-auto-instrumentation-java   ← 把 javaagent.jar 複製進共用 volume
Containers:
  app:
    Environment:
      JAVA_TOOL_OPTIONS: -javaagent:/otel-auto-instrumentation-java/javaagent.jar
      OTEL_SERVICE_NAME: my-app
      OTEL_EXPORTER_OTLP_ENDPOINT: http://gateway-collector:4318
      OTEL_TRACES_SAMPLER: parentbased_traceidratio
```

> 注意這份 CR 本身 apply 完什麼都不會發生——它不建立任何 Pod，只是躺在那裡等 webhook 來讀。這也是為什麼升級全公司 Java agent 只要改它的一個 image tag。

### 各語言的邊界條件

一份 CR 寫了所有語言，不代表全部都會注入——注入哪種語言由 Pod 的 annotation 決定，CR 只是規格書。但有幾個邊界要知道：

**1. Go 和 Nginx 的 feature gate 預設是關的**

Operator 端（`internal/config/config.go` 的預設值）：Java、NodeJS、Python、.NET、Apache HTTPD 預設開啟；**Go 和 Nginx 預設關閉**。要用的話 operator 啟動參數要加：

```
--enable-go-instrumentation=true
--enable-nginx-instrumentation=true
```

沒開就下 annotation 的話，webhook 會拒絕注入並對 Pod 發一個 `InstrumentationRequestRejected` 的 Warning event（Pod 照常建立，只是沒被插樁）——排查「為什麼沒注入」時先 `kubectl describe pod` 看 event。

**2. Go 還需要額外的 annotation，而且要特權**

Go 不是 init container 塞 agent，而是跑一個 eBPF sidecar，必須指定要掛哪個執行檔：

```yaml
instrumentation.opentelemetry.io/inject-go: "opentelemetry/company-default"
instrumentation.opentelemetry.io/otel-go-auto-target-exe: "/app/myserver"
```

這個 sidecar 需要 privileged 權限，安全審查上要另外評估。不確定值不值得的話，Go 服務用手動 SDK + `inject-sdk` 也是常見選擇。

**3. 同一個 Pod 多容器、多語言**

multi-instrumentation 預設開啟。Pod 裡有多個容器要注入不同語言時，用 container-names 系列 annotation 指定各語言對應到哪些容器：

```yaml
instrumentation.opentelemetry.io/inject-java: "opentelemetry/company-default"
instrumentation.opentelemetry.io/java-container-names: "api"
instrumentation.opentelemetry.io/inject-python: "opentelemetry/company-default"
instrumentation.opentelemetry.io/python-container-names: "worker"
```

**4. PHP 目前不支援自動注入**

`InstrumentationSpec` 沒有 `php` 欄位、也沒有 `inject-php` annotation（repo 裡 `autoinstrumentation/php/` 只有 Dockerfile，`versions.txt` 的 PHP 那行還是註解狀態）。PHP 服務的替代方案是 `inject-sdk`——它不裝任何 agent，但會把 CR 裡定義的 `OTEL_EXPORTER_OTLP_ENDPOINT`、`OTEL_SERVICE_NAME`、`OTEL_RESOURCE_ATTRIBUTES`、propagators、sampler 等環境變數注入 Pod：

```yaml
instrumentation.opentelemetry.io/inject-sdk: "opentelemetry/company-default"
```

前提是 PHP image 自己裝好 `opentelemetry` PECL extension 與 `open-telemetry/opentelemetry-auto-*` 套件，並設 `OTEL_PHP_AUTOLOAD_ENABLED=true`，PHP SDK 就會吃這些注入的 env。這樣至少「送去哪、掛什麼標籤、怎麼取樣」仍然由平台的 CR 集中治理，只有 agent 安裝這一步留在 image 裡。想再幫每個 PHP Pod 掛一個 sidecar collector 就近收資料的話，見下面「組合技」一節。

**5. image 欄位可以整個省略**

各語言的 `image` 不寫時，webhook 的 defaulter（`internal/webhook/instrumentation_webhook.go`）會自動填入與 operator 版本相符的預設 image。要鎖版本就明確 pin，但記得跟著 operator 一起升——pin 一個太舊的 agent（例如 java 2.10.0 對上 operator 0.153.0 預設的 2.28.1）等於放棄了「升級只改一個 tag」的治理紅利。

### 組合技:inject-sdk + sidecar collector

完整的分層架構長這樣:

```
app(SDK,localhost 直送)
  → sidecar collector(同 Pod;職責:填入 k8s 資訊)
    → agent collector(LB 層,Service 負載均衡)
      → gateway collector(集中處理:filter、transform、routing)
        → ELK / Loki / Prometheus / Tempo
```

把兩種注入疊在同一個 Pod 上即可——`inject-sdk` 管「應用怎麼送」,`sidecar.opentelemetry.io/inject` 管「Pod 裡有沒有 collector 接」,兩者由同一個 pod mutation webhook 分別處理:

```yaml
spec:
  template:
    metadata:
      annotations:
        instrumentation.opentelemetry.io/inject-sdk: "opentelemetry/company-sidecar"
        sidecar.opentelemetry.io/inject: "opentelemetry/php-sidecar"
```

兩個 annotation 的值都支援 `"namespace/name"` 跨 namespace 引用(sidecar 的解析在 `pkg/sidecar/podmutator.go` 的 `getCollectorInstance`)。

**陷阱:endpoint 必須留空,否則 SDK 會繞過 sidecar**

`company-default` 的 `exporter.endpoint` 指向 gateway,掛了 sidecar 的 Pod 如果還引用它,SDK 會直送 gateway、sidecar 白裝。程式碼行為(`internal/instrumentation/exporter.go` 的 `configureExporter`):CR 的 endpoint 非空才注入 `OTEL_EXPORTER_OTLP_ENDPOINT`;**留空就不注入,SDK 落回自己的預設值 `http://localhost:4318`——正好打到同 Pod 的 sidecar**。所以走 sidecar 的服務要用另一份 Instrumentation CR:

```yaml
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: company-sidecar     # 給掛 sidecar 的服務用
  namespace: opentelemetry
spec:
  exporter: {}              # endpoint 留空 → SDK 預設 localhost:4318 → sidecar
  propagators: [tracecontext, baggage]
  sampler:
    type: parentbased_traceidratio
    argument: "0.25"
```

**sidecar 的核心職責:填入 k8s 資訊——而且 operator 已經把材料備好了**

operator 注入 sidecar 時,會自動幫 sidecar 容器(`otc-container`)掛上 downward API env 和一個組好的 `OTEL_RESOURCE_ATTRIBUTES`,內容包含 `k8s.pod.name`、`k8s.pod.uid`、`k8s.node.name`、`k8s.namespace.name`,還會查 owner reference 補上 `k8s.deployment.name/uid`、`k8s.replicaset.name/uid`(`pkg/sidecar/attributes.go` 的 `getResourceAttributesEnv`)。所以 sidecar 的 config 只需要一個 `resourcedetection`(env detector)就能把這些屬性蓋章到所有經過的資料上——**不需要打 k8s API、不需要 RBAC、沒有 k8sattributes 的 cache 開銷**。

這也回答了「為什麼 k8s enrichment 要放在 sidecar 這一層」:`k8sattributes` processor 靠連線來源的 pod IP 反查 metadata,資料一旦經過 agent(LB)轉手,gateway 看到的來源就是 agent 的 IP,反查不回原始 Pod。sidecar 是唯一「天生知道資料來自哪個 Pod」的一層。

```yaml
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: php-sidecar
  namespace: opentelemetry
spec:
  mode: sidecar
  config:
    receivers:
      otlp:
        protocols:
          grpc: {}          # localhost:4317
          http: {}          # localhost:4318,收同 Pod SDK 打來的資料
    processors:
      memory_limiter:
        check_interval: 1s
        limit_mib: 100
        spike_limit_mib: 25
      # 讀 operator 自動注入的 OTEL_RESOURCE_ATTRIBUTES,
      # 把 k8s.pod.name / k8s.deployment.name 等蓋章到所有 signal 上
      resourcedetection:
        detectors: [env]
        override: false
      batch:
        timeout: 2s         # per-pod 流量小,批次時間短
    exporters:
      otlp/agent:
        # 下一層是 agent collector(LB 層),走 Service 負載均衡
        endpoint: otel-agent-collector.observability.svc:4317
        tls:
          insecure: true
    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [memory_limiter, resourcedetection, batch]
          exporters: [otlp/agent]
        metrics:
          receivers: [otlp]
          processors: [memory_limiter, resourcedetection, batch]
          exporters: [otlp/agent]
        logs:
          receivers: [otlp]
          processors: [memory_limiter, resourcedetection, batch]
          exporters: [otlp/agent]
```

資料流:PHP SDK(讀注入的 `OTEL_*` env,送 `localhost:4318`)→ sidecar(補 k8s 屬性)→ agent(LB)→ gateway(filter/transform/routing)→ ELK / Loki / Prometheus / Tempo。後端地址只在 gateway 設定一次,sidecar 和 agent 都不需要知道。

順帶一提:走 operator 自動注入(`inject-java` 等)的服務,SDK 的 env 裡本來就帶了大部分 k8s.* 屬性(webhook 的 `createResourceMap` 塞的);sidecar 的 `resourcedetection` 設 `override: false`,已有的屬性不會被蓋掉,主要是兜底那些沒經過自動注入的 signal。

**成本提醒**:每個 Pod 掛一個 sidecar collector,容器數與資源開銷隨服務數線性增長。這個代價買到的是:per-pod 的 k8s 身分標記(如上)、每個服務可以有不同的 collector 處理邏輯(獨立遮罩、租戶隔離),以及 PHP-FPM 這類短生命週期 process 就近 flush 的可靠性(process 結束前打 localhost 比打遠端可靠)。如果這些都用不上、sidecar 只是原封不動轉發,讓 SDK 直送 agent 層即可,少一層元件。

### 落地:全公司服務的接入矩陣

實際推行時,存量服務的狀態不會整齊——有的完全沒接 OTel、有的團隊早就手動裝了 SDK、還有 PHP。這些情況可以收斂成一套統一模型:**sidecar annotation 人人都加(k8s 資訊由它統一蓋章),差別只在 Instrumentation 這邊用哪種 inject annotation**。

| 服務類型 | Instrumentation annotation | sidecar annotation | 團隊要做的事 |
|---|---|---|---|
| 無 OTel(Java/Python/Node) | `inject-<lang>` | 要 | 加兩行 annotation |
| 已手動裝 javaagent(Dockerfile 塞的) | 改用 `inject-<lang>` | 要 | 從 image 移除 agent |
| 已手動裝 SDK(程式內建 provider) | `inject-sdk` | 要 | 移除硬編的 provider 配置,改 env 驅動 |
| PHP | `inject-sdk` | 要 | image 裝 PECL extension |

四類服務都引用同一份 `company-sidecar` Instrumentation CR(endpoint 留空 → SDK 打 localhost → sidecar),k8s 資訊、上報路徑、取樣策略全部由平台集中治理。

**紅線:已經手動插樁的服務,絕對不要加 `inject-<lang>`**

operator 的防重複檢查(`internal/instrumentation/podmutator.go` 的 `isAutoInstrumentationInjected`)只認得**自己**注入過的東西,不知道 image 裡已經有手動裝的 agent/SDK。加了就是雙重插樁:span 重複、context 錯亂;Java 更慘——operator 是往既有的 `JAVA_TOOL_OPTIONS` 後面 append `-javaagent`,兩個 agent 同時掛上。

**「移除手動配置」的正確理解:不是移除 SDK,是移除硬編的 provider 配置**

已手動裝 SDK 的服務,SDK 和業務程式碼裡的 OTel API 呼叫(自訂 span、metric)都保留——要改的只有 provider 初始化。所有語言的 SDK 都支援標準 `OTEL_*` 環境變數驅動(Java 用 autoconfigure,Python/Node 預設就吃 env),但程式碼裡硬編的 endpoint/sampler/resource 會蓋掉 env。所以請團隊把 provider 初始化改成「無參數、由 SDK 自動從 env 配置」,之後這些值全部由平台的 CR 下發,團隊不再需要碰。

**過渡期可以逐服務漸進,不用一刀切**

operator 注入 env 用的是 append-if-not-set 語意(`configureExporter`、`createResourceMap` 都是既有值優先)——團隊自己 manifest 裡設的 `OTEL_*` env 不會被 operator 蓋掉。還沒改完 provider 配置的服務先掛 sidecar annotation(至少拿到 k8s 屬性蓋章),等程式碼改完 env 驅動再補 `inject-sdk`,平台接管配置。

這個模型的終點,就是前言講的治理目標:**團隊只碰業務程式碼和 OTel API,SDK 的一切配置歸平台**。API 與 SDK 的分界正是 OTel 設計上留給平台治理的縫——團隊寫 API 呼叫表達「我要記什麼」,平台透過 CR 決定「送去哪、怎麼取樣、掛什麼標籤」。

## OpAMPBridge — Collector 群的遠端控制面

角色：在 cluster 裡部署一個 bridge（agent），透過 OpAMP 協議連到遠端的 OpAMP server，把「這個 cluster 裡 Operator 管的所有 Collector」代理給控制面。

功能：Operator 依 CR 建出 bridge 的 Deployment。bridge 會回報 cluster 內各 Collector 的狀態與有效設定（reportsEffectiveConfig），並能接受 server 下推的新設定（acceptsRemoteConfig）、回寫到對應的 OpenTelemetryCollector CR——等於不碰 kubectl 就能遠端改 Collector 的設定和版本。componentsAllowed 可以限制遠端只能下推哪些 receiver/processor/exporter，是控制面自己的安全閥。

治理意義：前面兩個 CR 解決的是「單一 cluster 內」的治理，OpAMPBridge 把治理面再拉高一層——跨 cluster、跨環境的集中管控。對管十幾個 cluster 的平台團隊，這是從「每個 cluster 各自 GitOps」走向「一個控制面看到並管理所有 Collector 的設定與版本」的路。這正是你前言講的第二層痛點（Collector 管不動）在多 cluster 尺度上的答案。

```yaml=
apiVersion: opentelemetry.io/v1alpha1
kind: OpAMPBridge
metadata:
  name: opampbridge
spec:
  endpoint: ws://opamp-server:4320/v1/opamp   # 遠端 OpAMP server
  capabilities:
    ReportsEffectiveConfig: true   # 上報各 Collector 的實際設定
    AcceptsRemoteConfig: true      # 接受遠端下推的新設定
    ReportsHealth: true
  componentsAllowed:               # 安全閥：遠端只准下推這些元件
    receivers: [otlp]
    processors: [memory_limiter, batch]
    exporters: [otlphttp]
```

apply 之後 Operator 建出一個 `bridge` 的 Deployment。它會列出 cluster 裡所有帶對應 label 的 `OpenTelemetryCollector CR`、把有效設定回報給遠端 server；server 下推新設定時，bridge 直接改寫對應的 CR——後續的滾動更新就交回給 Operator。效果：不碰 kubectl、不進 cluster，就能遠端改 Collector 的設定和版本。

## TargetAllocator — Prometheus 抓取目標的調度者

角色：解決一個很具體的問題：Collector 用 prometheus receiver 抓 metrics 時，如果跑多副本，每個副本都拿同一份 scrape config，就會每個 target 被抓 N 次。TargetAllocator 是一個獨立服務，把 scrape targets 用一致性雜湊等策略（allocationStrategy）分配到各 Collector 副本，讓抓取工作可以水平擴展。

功能：Operator 建出 TargetAllocator 的 Deployment；各 Collector 副本改成向它查詢「我該抓哪些 target」。它還能啟用 prometheusCR，直接認得 Prometheus Operator 的 ServiceMonitor/PodMonitor——現有的 Prometheus 生態不用重寫就能接進 OTel 管線。它可以獨立成 CR，也可以直接內嵌在 OpenTelemetryCollector 的 spec.targetAllocator 裡。

治理意義：四個裡面治理色彩最淡、偏基礎設施能力的一個。它的價值在遷移路徑：讓平台把散落各處的 Prometheus 抓取收編進統一的 Collector 管線，而各團隊已經寫好的 ServiceMonitor 照用——降低「收編」的阻力。

```yaml=
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: metrics
spec:
  mode: statefulset
  replicas: 3
  targetAllocator:
    enabled: true
    prometheusCR:
      enabled: true    # 直接吃現有的 ServiceMonitor / PodMonitor
  config:
    receivers:
      prometheus:
        config: {}     # scrape 目標由 Tar 寫
    ...
```

apply 之後多出一個 `metrics-targetallocator` Deployment，三個 Collector 副本開機時改成問它「我該抓哪些 target」，同一個 target 只會被一個副本抓。各團隊已經寫好的 ServiceMonitor 一個都不用改。

https://grafana.com/blog/demystifying-the-opentelemetry-operator-observing-kubernetes-applications-without-writing-code/

https://opentelemetry.io/docs/platforms/kubernetes/operator/
