# Stage 3：替 Java 服務注入 Sidecar Collector + Auto-Instrument

> **本階段目標：** 不改一行 Java 程式碼，只靠在 Pod 加 annotation，讓 Operator 自動替 order-service 注入「sidecar collector + Java auto-instrumentation」。做完之後，跨服務 trace 不再斷在 payment → order。

---

## 3.1 這一階段要加的兩個東西

```
order-service Pod（注入後）
┌───────────────────────────────────────────────────────┐
│  initContainer: opentelemetry-auto-instrumentation     │  ← Operator 注入
│    把 opentelemetry-javaagent.jar 複製進共享 volume      │
│                          │                              │
│                          ▼ emptyDir 共享                 │
│  ┌─────────────────────────────────┐                   │
│  │ order-service (app container)   │                   │
│  │  + JAVA_TOOL_OPTIONS=            │  ← Operator 注入   │
│  │      -javaagent:/otel-auto.../   │     env           │
│  │  + OTEL_* 一堆環境變數            │                   │
│  │        │ OTLP localhost:4318     │                   │
│  └────────┼────────────────────────┘                   │
│           ▼                                             │
│  ┌─────────────────────────────────┐                   │
│  │ otc-container (sidecar collector)│  ← Operator 注入   │
│  │  來自 app-sidecar CR             │     (batch→agent) │
│  └─────────────────────────────────┘                   │
└───────────────────────────────────────────────────────┘
```

這些都由 Pod template 上的 annotation 觸發，三個各司其職：

| annotation | 值代表什麼 | 效果 |
|---|---|---|
| `sidecar.opentelemetry.io/inject` | 要注入的 **sidecar collector CR 名稱**（`app-sidecar`） | 注入一個 sidecar collector container |
| `instrumentation.opentelemetry.io/inject-java` | 要套用的 **Instrumentation CR 名稱**（`lab-instrumentation`） | 注入 javaagent initContainer + 一堆 `OTEL_*` env |
| `instrumentation.opentelemetry.io/container-names` | 要注入的 **app container 名稱清單**（逗號分隔） | 限定 instrumentation 注入到哪幾個 container |

> **背景知識展開（classroom 第 4、6 章）：這三個 annotation 為什麼「一 apply 就生效」？**
>
> Pod 建立請求送到 API Server 時，會先經過 Operator 註冊的 **Mutating Admission Webhook**（[`internal/webhook/podmutation/webhookhandler.go`](../../internal/webhook/podmutation/webhookhandler.go)），流程分三層：
>
> ```
> ① HTTP 層  webhookhandler.go     解碼 Pod → 呼叫每個 PodMutator.Mutate()
> ② 決策層  instrumentation/podmutator.go
>            讀 annotation → 依名稱/true/false/namespace 規則找到對應的 Instrumentation CR
> ③ 執行層  instrumentation/sdk.go → javaagent.go / python.go ...
>            實際修改 Pod spec：加 initContainer、加 env、加 volume
> ```
>
> 這一步是**同步**發生的：Pod 在真正建立前，Webhook 已經把修改後的版本算成 JSON Patch 回傳給 API Server，所以你 `kubectl apply` 完馬上 `kubectl get pod -o yaml` 就能看到注入結果，不需要等任何非同步流程。
>
> 三個 annotation 分別對應決策層裡的三個判斷：`sidecar.opentelemetry.io/inject` 決定要不要疊加 sidecar collector（另一條獨立的 mutator 邏輯）；`inject-java` 決定要不要跑 Java 的注入邏輯（`javaagent.go`）；`container-names` 則是傳給執行層，告訴它「改哪個 container 的 env/volumeMounts」。
>
> 這裡是把 classroom 第 4、6 章講過的機制實際跑起來。3.3 會把這三個 annotation 逐一拆解。

---

## 3.2 先建立 Instrumentation CR 與 sidecar 範本

完整檔案：[`manifests/20-instrumentation.yaml`](./manifests/20-instrumentation.yaml)。重點：

```yaml
# sidecar collector：app 的本地 OTLP 端點，batch 後轉送給 Stage 2 的 agent
kind: OpenTelemetryCollector
metadata: { name: app-sidecar }
spec:
  mode: sidecar
  config:
    exporters:
      otlp/agent:
        endpoint: agent-collector.otel-lab.svc.cluster.local:4317
---
# Instrumentation：定義 auto-instrument 時要設哪些 OTEL_* 環境變數
kind: Instrumentation
metadata: { name: lab-instrumentation }
spec:
  exporter:
    endpoint: http://localhost:4318   # SDK → 同 Pod 的 sidecar
  sampler:
    type: parentbased_always_on       # SDK 端全收，取樣交給 gateway tail_sampling
```

```bash
kubectl apply -f manifests/20-instrumentation.yaml

kubectl -n otel-lab get instrumentation lab-instrumentation
kubectl -n otel-lab get opentelemetrycollector app-sidecar
```

**`Instrumentation` CR 到底是什麼？**

它是 operator 四大 CR 之一（一個 Kubernetes 自訂資源），內容是**「auto-instrument 時要套用的一套 OTel 設定範本」**——telemetry 送去哪（`exporter.endpoint`）、怎麼取樣（`sampler`）、要灌哪些 `OTEL_*` 環境變數（`env` 與各語言的 `java:` / `python:` 區塊）。它**本身不會跑任何東西、也不產生 Pod**，就只是「一份設定」，放在那裡等別人來引用。

```
Instrumentation CR (lab-instrumentation)         ← 一份「注入設定範本」
  spec:
    exporter.endpoint  ─┐
    sampler            ─┤  注入時，operator 把這些
    env / java / python ┘  變成 Pod 裡的 OTEL_* env + javaagent
```

> 同理，上面的 `app-sidecar`（`OpenTelemetryCollector` mode: sidecar）也只是個「sidecar collector 範本」，apply 後不會立刻產生任何 Pod——要等有 Pod 帶對應 annotation 時，operator 才會照範本把 collector 注進那個 Pod。
>
> **這兩個 CR 都是「被引用的範本」，真正觸發注入的是下一步 Pod 上的 annotation。**

---

## 3.3 套用「已注入」版本的 order-service

[`manifests/21-order-service-instrumented.yaml`](./manifests/21-order-service-instrumented.yaml) 和 Stage 1 的 `02-` 版本**唯一差別**就是多了三個 annotation：

```yaml
template:
  metadata:
    annotations:
      sidecar.opentelemetry.io/inject: "app-sidecar"
      instrumentation.opentelemetry.io/inject-java: "lab-instrumentation"
      instrumentation.opentelemetry.io/container-names: "order-service"
```

**三個 annotation 逐一拆解：**

**① `sidecar.opentelemetry.io/inject: "app-sidecar"`**
- 作用：要 Operator 注入一個 **sidecar collector** 到這個 Pod。
- 值的意義：是一個 **`OpenTelemetryCollector`(mode: sidecar) CR 的名稱**——這裡指向 3.2 建立的 `app-sidecar`。Operator 會拿那個 CR 的 config 起一個 collector container 塞進來。
- 其他可用的值：`"true"`（找同 namespace 唯一的 sidecar CR）、`"<namespace>/<name>"`（跨 namespace 指名）、`"false"`（不注入）。

**② `instrumentation.opentelemetry.io/inject-java: "lab-instrumentation"`**
- 作用：要 Operator 對這個 Pod 做 **Java auto-instrumentation**——注入一個把 `javaagent.jar` 複製進來的 initContainer，並在 app container 補上 `JAVA_TOOL_OPTIONS=-javaagent:...` 和一堆 `OTEL_*` env。
- **值是「`Instrumentation` CR 的名稱」，不是 `true`。** `"lab-instrumentation"` 就是指向 3.2 那個 CR，用它裡面的 endpoint / sampler / env 來注入。四種可填的值：

  | 值 | 意義 |
  |---|---|
  | `"lab-instrumentation"` | **指名**用這個 Instrumentation CR（lab 用的就是這種） |
  | `"true"` | **捷徑**：自動找「同 namespace 裡唯一的」Instrumentation CR；有多個 CR 時不能用這個 |
  | `"<namespace>/<name>"` | 跨 namespace 指名一個 CR |
  | `"false"` | 不注入 |

- 語言對應 annotation：Java 是 `inject-java`，Python 是 `inject-python`（Stage 4 會用到），其餘還有 `inject-nodejs` / `inject-dotnet` / `inject-go` 等，值的語意都一樣。

**③ `instrumentation.opentelemetry.io/container-names: "order-service"`**
- 作用：**限定 instrumentation 要注入到哪幾個 container**（逗號分隔多個，例如 `"app1,app2"`）。
- 為什麼需要：注入發生時，Pod 裡其實會有多個 container（你的 app + 被注入的 `otc-container` sidecar collector）。如果不指定，Operator 預設只對「第一個」container 注入，多 container 時容易注入錯對象。明確寫出 app container 名稱是最穩的做法。
- 注意：這裡填的是 **app container 的名稱**（`order-service`），不要把被注入的 `otc-container` 寫進去——你不會想對 collector 本身再注入一次 javaagent。

**annotation 與 CR 怎麼接起來（②為例）：**

```
   Pod annotation                                      Instrumentation CR (3.2 建的)
   inject-java: "lab-instrumentation"  ──指向──▶  metadata.name: lab-instrumentation
                                                    spec: { exporter, sampler, env, java... }
                                                             │
                       operator 的 webhook 讀這份 spec，把對應的 OTEL_* env + javaagent 注進 Pod
```

**為什麼要拆成「CR + annotation」兩層？**
- **CR（`Instrumentation` / sidecar `OpenTelemetryCollector`）** = 「一套設定範本」，集中寫一次。
- **annotation** = 「哪個 Pod 要被注入、用哪一套範本」。

好處：50 個服務共用同一套設定時，CR 只寫一份；各服務只要貼一行 annotation 指向它。要改 endpoint / 取樣，改 CR 一處，所有引用它的 Pod 下次重建就生效——這正是 Stage 4 會講的「設定集中」。

```bash
kubectl apply -f manifests/21-order-service-instrumented.yaml
kubectl -n otel-lab rollout status deployment/order-service
```

---

## 3.4 驗證注入確實發生

**(1) 注入了 sidecar collector 與 javaagent initContainer：**

在 Kubernetes 1.28+（本 lab 的 k3s 1.31 即是）Operator 會用 **native sidecar**——也就是
一個 `restartPolicy: Always` 的 **initContainer**——來注入 collector，而不是放在 `containers`。
所以 sidecar collector（`otc-container`）會出現在 `initContainers` 而不是 `containers`：

```bash
# app container：仍只有 order-service
kubectl -n otel-lab get pod -l app=order-service \
  -o jsonpath='{.items[0].spec.containers[*].name}{"\n"}'
# 預期：order-service

# initContainers：otc-container（native sidecar）+ java agent 複製器
kubectl -n otel-lab get pod -l app=order-service \
  -o jsonpath='{range .items[0].spec.initContainers[*]}{.name}({.restartPolicy}){" "}{end}{"\n"}'
# 預期：otc-container(Always) opentelemetry-auto-instrumentation-java()
#   ↑ restartPolicy=Always 代表它是「會持續運行」的 native sidecar，不是跑完就結束的 init
```

> 觀念釐清：native sidecar 在 spec 上歸類於 `initContainers`，但因為 `restartPolicy: Always`，
> 它會在 app container 之前啟動、並在整個 Pod 生命週期持續運行——行為等同傳統 sidecar。
> `kubectl get pod` 的 READY 欄位會把它算進去（你會看到 `2/2`）。

驗證 Pod 是 2/2：

```bash
kubectl -n otel-lab get pod -l app=order-service
# READY 應為 2/2（app + native sidecar 都算）
```

**(3) app container 被注入了 javaagent 與一堆 OTEL_* env：**

```bash
kubectl -n otel-lab get pod -l app=order-service -o yaml \
  | grep -E 'JAVA_TOOL_OPTIONS|OTEL_SERVICE_NAME|OTEL_EXPORTER_OTLP_ENDPOINT|OTEL_EXPORTER_OTLP_PROTOCOL' -A1 | head -20
# 預期看到：
#   JAVA_TOOL_OPTIONS:  -javaagent:/otel-auto-instrumentation-java/javaagent.jar
#   OTEL_SERVICE_NAME:  order-service   ← Operator 從 Deployment 名稱自動推導
#   OTEL_EXPORTER_OTLP_ENDPOINT: http://localhost:4318
#   OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf
```

> **`OTEL_SERVICE_NAME` 怎麼推導出來的？（背景知識展開，classroom 第 6 章）** Operator 的 `chooseServiceName()`（[`internal/instrumentation/sdk.go`](../../internal/instrumentation/sdk.go) 第 528 行）依序嘗試以下來源，找到第一個非空值就用它：
>
> ```
> 1. Pod annotation：resource.opentelemetry.io/service.name
> 2. Pod label：app.kubernetes.io/name（需開啟 useLabelsForResourceAttributes）
> 3~8. 所屬 Deployment → ReplicaSet → StatefulSet → DaemonSet → CronJob → Job 的名稱
> 9. Pod 名稱
> 10. Container 名稱（最後手段）
> ```
>
> 這份 lab 沒有特別設 annotation 或 `app.kubernetes.io/name` label，所以落到第 3 層——`order-service` 這個名字是 Operator 查 Pod 的 `ownerReference` 反推出 Deployment 名稱得到的，不是寫死在哪份設定裡。

---

## 3.5 打流量，確認 Java 端終於有 span 了

```bash
# （若 Stage 1 的 port-forward 還在就略過）
kubectl -n otel-lab port-forward svc/payment-service 5000:5000 &
sleep 3

curl -s -X POST "http://localhost:5000/pay?item=pen&amount=2" >/dev/null
sleep 8   # 等資料流過 sidecar → agent → gateway，並通過 tail_sampling 的 decision_wait
```

**重點：在哪裡看 span？** order-service 的 sidecar（`otc-container`）設定是把資料往上轉送到 agent，
它**自己沒有 debug exporter**，所以看它的 log 不會有 span。實際印出 span 的是最末端的 **gateway**
（它的 exporter 是 `debug`）。

**讀 gateway log 的兩個坑（務必照這個方式讀，否則常常 grep 到空的）：**

1. gateway 有 **2 個副本**，loadbalancing 會把不同 trace 分到不同副本，所以要**兩個 pod 都讀**。
2. `kubectl logs -l <label>`（用 label 選取）會預設只抓每個 pod 最後 10 行，`--since` 會被這個限制吃掉。
   用 **pod 名稱**逐一讀（named pod 預設抓全部）才正確。

把「讀兩個 gateway 副本」包成一個小函式，後面都用它：

```bash
gwlogs() {  # 用法：gwlogs 25s
  for p in $(kubectl -n otel-lab get pods -l app.kubernetes.io/instance=otel-lab.gateway -o name); do
    kubectl -n otel-lab logs "$p" --since="${1:-20s}" 2>/dev/null
  done
}

# tail_sampling 有 decision_wait=10s，打完流量要等一下再看
gwlogs 25s | grep -E 'Name +: (POST /orders|INSERT labdb)' | sort | uniq -c
# 預期看到（次數對應你打的流量數）：
#   N  Name           : INSERT labdb.order_record  ← JDBC span（Java agent 自動產生）
#   N  Name           : POST /orders               ← Spring MVC server span
```

> 看到 `INSERT labdb.order_record` 這個 JDBC span，代表 Java agent 連「app 對 PostgreSQL 的查詢」都自動補上了 span——而 order-service 的程式碼完全沒碰過 OpenTelemetry。
>
> 跨服務 trace 之所以能接起來，是因為 Python 的 `requests` instrumentation 在 HTTP header 注入了 W3C `traceparent`，Java agent 自動讀取它當作 parent context（propagators 在 Instrumentation CR 設成 `tracecontext,baggage`）。完整的「payment + order 同一條 trace」會在 Stage 4 把 payment 也接上 gateway 後一次看到。

---

## 3.6 此時的完整資料流

```
payment-service ──(手動 sidecar，Stage 4 會換掉)──┐
                                                 │
order-service ──(注入的 sidecar otc-container)────┤
                                                 ▼
                                    agent-collector (loadbalancing)
                                                 │ routing_key: traceID
                                                 ▼
                                    gateway-collector x2 (tail_sampling: error/slow 全留 + 其餘 10%)
                                                 │
                                                 ▼  debug exporter
```

注意：order-service 的 sidecar 已經把資料送進 Stage 2 的 agent → gateway 了。確認 gateway 有收到（沿用上面的 `gwlogs` 函式）：

```bash
curl -s -X POST "http://localhost:5000/pay?item=check&amount=1" >/dev/null; sleep 12
gwlogs 20s | grep -c 'order-service'
# 預期 > 0
```

---

## 練習 3

**動手題：** 故意把 `21-order-service-instrumented.yaml` 的 `instrumentation.opentelemetry.io/inject-java` 值打錯成一個不存在的 Instrumentation 名稱（例如 `nope`），重新 apply，觀察 Pod 是否能起來、Operator/webhook 有什麼反應。修正後再 apply 一次。

**閱讀理解題：** order-service 的 app container image 跟 Stage 1 完全一樣（`order-service:lab`，沒有掛任何 javaagent）。那 `-javaagent` 到底是「什麼時候」、由「誰」加上去的？

<details>
<summary>參考答案</summary>

**打錯名稱：** webhook 找不到對應的 Instrumentation CR，注入會失敗。視 Operator 版本，可能 Pod 仍以「未注入」狀態起來（annotation 被忽略並記 event/log），或 admission 被拒。重點是：注入是 webhook 在 **Pod 建立的 admission 階段** 動態完成的，CR 不存在就沒得注入。

**javaagent 何時、由誰加：** image 本身沒有 javaagent。是 MutatingWebhook 在 Pod 建立請求進入 API server 時，對 Pod spec 做 JSON Patch：
1. 加一個 initContainer，把 javaagent.jar 從 instrumentation image 複製到 emptyDir
2. 在 app container 注入 `JAVA_TOOL_OPTIONS=-javaagent:...` 環境變數，JVM 啟動時讀到它就掛上 agent

所以「零改 code、零改 image」，全靠 admission 階段的動態 patch——這裡示範的是 Java 的 initContainer 模式；對應 classroom 第 4 章（[`internal/webhook/podmutation/webhookhandler.go`](../../internal/webhook/podmutation/webhookhandler.go) 的 JSON Patch 機制）與第 6 章（[`internal/instrumentation/javaagent.go`](../../internal/instrumentation/javaagent.go) 的 Java 注入細節；Stage 4 會看到 Python 用 `PYTHONPATH` 而不是 initContainer + agent 的方式，Go 甚至改用 eBPF sidecar，三者機制完全不同）。
</details>

---

## 附錄：`inject-*` annotation 速查表

本 lab 只用到 `inject-java`（本章）與 `inject-python`（Stage 4），但 Operator 一共支援 8 種注入。以下版本是**這份 repo（`versions.txt`）的預設值**，實際以你裝的 Operator 版本為準。

| annotation | 注入什麼 | 預設版本 | 機制 |
|---|---|---|---|
| `instrumentation.opentelemetry.io/inject-java` | Java agent | 2.28.1 | initContainer 複製 `javaagent.jar` → `JAVA_TOOL_OPTIONS=-javaagent:...` |
| `instrumentation.opentelemetry.io/inject-nodejs` | Node.js | 0.76.0 | `NODE_OPTIONS=--require ...` |
| `instrumentation.opentelemetry.io/inject-python` | Python | 0.63b1 | `PYTHONPATH` + `sitecustomize.py` |
| `instrumentation.opentelemetry.io/inject-dotnet` | .NET | 1.15.0 | `CORECLR_*` profiler env |
| `instrumentation.opentelemetry.io/inject-go` | Go | v0.24.0 | **eBPF**（需 sidecar + 提權，且要指定 `…/otel-go-auto-target-exe`） |
| `instrumentation.opentelemetry.io/inject-apache-httpd` | Apache HTTPD module | 1.0.4 | 注入 OTel module |
| `instrumentation.opentelemetry.io/inject-nginx` | Nginx module | 1.0.4 | 注入 OTel module |
| `instrumentation.opentelemetry.io/inject-sdk` | **只注入 SDK 環境變數**（不掛 agent） | — | 給「已自己 instrument、但想讓 Operator 統一灌 `OTEL_*` / resource」的 app |

**共通規則：**
- **值的語意 8 種都一樣**：`"<name>"`（指名 Instrumentation CR）/ `"true"`（同 namespace 唯一 CR 的捷徑）/ `"<namespace>/<name>"`（跨 namespace）/ `"false"`（不注入）。
- **可以同時注入多種語言**，但只要啟用超過一種，`container-names`（或語言專屬版如 `java-container-names`、`python-container-names`）就**必填**——Operator 會擋下「多語言卻沒指定 container」的設定。
- **語言專屬的輔助 annotation**：`…/otel-python-platform`（glibc/musl）、`…/otel-dotnet-auto-runtime`、`…/otel-go-auto-target-exe`（Go eBPF 必填）。

---

| | |
|---|---|
| 上一步 | [← Stage 2](./02-collector-gateway-and-loadbalancer.md) |
| 下一步 | [Stage 4：把 Python 從手動改成 Operator 管理 →](./04-python-migrate-to-operator.md) |
