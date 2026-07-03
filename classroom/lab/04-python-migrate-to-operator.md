# Stage 4：把 Python 從「手動 OTel」遷移到「Operator 管理」

> **本階段目標：** 把 payment-service 原本「手動裝的 SDK + 自掛 sidecar collector」換成「Operator 注入的 auto-instrument + sidecar」。重點是遷移的「為什麼」與「怎麼避免雙重 instrumentation」。

---

## 4.1 遷移前後對照

```
   遷移前 (Stage 1)                         遷移後 (Stage 4)
┌──────────────────────────┐          ┌──────────────────────────────┐
│ payment-service Pod      │          │ payment-service Pod          │
│                          │          │                              │
│ container 1: app         │          │ initContainer:               │
│   - app.py 內手動 init    │          │   opentelemetry-auto-...-python│
│     OTel SDK              │   ───▶   │ container 1: app             │
│   - requirements 裡有     │          │   - app 不再自己 init OTel     │
│     opentelemetry-*       │          │   - Operator 注入 OTEL_* env  │
│                          │          │     + PYTHONPATH/sitecustomize│
│ container 2: 手動 sidecar │          │ container 2: 注入的 sidecar    │
│   collector (設定寫死)    │          │   (來自 app-sidecar CR)        │
└──────────────────────────┘          └──────────────────────────────┘
   設定散落、要改得動 app                  設定集中在 CR、app 乾淨
```

---

## 4.2 遷移要處理的三件事

| # | 動作 | 在這個 lab 怎麼做 | 真實專案怎麼做 |
|---|---|---|---|
| 1 | 移除手動 sidecar collector container | 新 manifest 不再宣告那個 container | 同左 |
| 2 | 關掉 app 內手動 SDK，避免雙重 instrumentation | 不再設 `MANUAL_OTEL=true`（app.py 用此 flag 控制） | 刪掉 app.py 的 `_init_manual_otel()`；requirements 移除 `opentelemetry-sdk`/`exporter`/`instrumentation-*`，**但保留 `opentelemetry-api`** |
| 3 | 加上注入 annotation | 加 `sidecar` + `inject-python` | 同左 |

> **為什麼「雙重 instrumentation」是大忌？** 如果 app 已經手動建了 TracerProvider，Operator 又注入 auto-instrument（也會設定一套 SDK），會出現兩套 provider 互搶、span 重複或衝突。遷移時務必擇一。這個 lab 用 `MANUAL_OTEL` 環境變數讓你能用同一個 image 對照前後，真實情況請直接刪手動 code。

---

## 4.2.1 ★ 自訂 metrics / log 怎麼活過遷移？（這題最容易踩雷）

payment-service 不只有 trace，還有 app code 主動寫的**自訂業務遙測**：

```python
# app.py —— 業務遙測一律走「global API」，不自己 new provider
meter = metrics.get_meter("payment-service")
payments_counter = meter.create_counter("payments.count", ...)
payment_amount   = meter.create_histogram("payments.amount", ...)
log = logging.getLogger("payment-service")
# /pay 內：
payments_counter.add(1, {"item": item})
payment_amount.record(amount, {"item": item})
log.info("payment processed: ...")
```

關鍵在於**「API vs SDK」的分工**：

```
   opentelemetry-api  (輕量，只有介面)        opentelemetry-sdk  (實作 + exporter)
   ─────────────────────────────────         ──────────────────────────────────
   meter.create_counter(...)                 誰真正把 counter 匯出去？
   log.info(...)                             誰建立 MeterProvider / LoggerProvider？
        │                                              │
        │  app code 只碰這層                            │  Stage 1：app 自己 new（_init_manual_otel）
        ▼                                              ▼  Stage 4：Operator 注入的 agent 提供
   global registry（set_meter_provider / set_logger_provider 決定背後是誰）
```

- 業務 code 只呼叫 **global API**（`metrics.get_meter()` / `logging`），不直接持有 provider。
- 「誰提供 SDK provider」是可抽換的：Stage 1 是 app 手動 new，Stage 4 換成 Operator 注入的 agent。
- 因為 API 介面不變，**自訂 counter / histogram / log 一行都不用改就活過遷移**。

**遷移規則：**

| requirements 套件 | 遷移後 | 原因 |
|---|---|---|
| `opentelemetry-api` | **保留** | 業務 code 寫自訂 metrics/log 要用它的介面 |
| `opentelemetry-sdk` | 移除 | provider 改由 agent 提供 |
| `opentelemetry-exporter-otlp-*` | 移除 | exporter 改由 agent 設定 |
| `opentelemetry-instrumentation-flask/requests/psycopg2` | 移除 | auto-instrument 自動涵蓋 |

> **常見災難：** 有人遷移時把 `opentelemetry-api` 也一起移掉 → app `import opentelemetry` 直接 `ImportError`；或是只刪了 sidecar、忘了關手動 provider → metrics 被匯出兩次、數字翻倍。記住：**留 API、去 SDK、關手動 provider**。

為了讓 agent 真的把這三種訊號送出去，[`20-instrumentation.yaml`](./manifests/20-instrumentation.yaml) 已經設好對應的 exporter 環境變數：

```yaml
spec:
  env:
    - { name: OTEL_TRACES_EXPORTER,  value: otlp }
    - { name: OTEL_METRICS_EXPORTER, value: otlp }   # ← 不設的話自訂 metrics 不會被匯出
    - { name: OTEL_LOGS_EXPORTER,    value: otlp }    # ← 不設的話自訂 log 不會被匯出
  python:
    env:
      - { name: OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED, value: "true" }  # 掛 logging handler
```

---

## 4.3 套用遷移後的版本

完整檔案：[`manifests/30-payment-service-operator.yaml`](./manifests/30-payment-service-operator.yaml)。重點：

```yaml
template:
  metadata:
    annotations:
      sidecar.opentelemetry.io/inject: "app-sidecar"
      instrumentation.opentelemetry.io/inject-python: "lab-instrumentation"
      instrumentation.opentelemetry.io/container-names: "payment-service"
  spec:
    containers:
      - name: payment-service       # 只剩 app，手動 sidecar 已移除
        env:
          # 沒有 MANUAL_OTEL=true → app.py 不再自己初始化 OTel
          - name: ORDER_SERVICE_URL
            value: http://order-service:8080
```

三個 annotation 的作用跟 [Stage 3 §3.3 的逐一拆解](./03-java-sidecar-and-autoinstrument.md#33-套用已注入版本的-order-service)完全一樣，唯一差別是把 `inject-java` 換成 **`inject-python`**（值一樣是 `Instrumentation` CR 名稱）。`container-names` 同樣指向 app container 名稱 `payment-service`。

```bash
kubectl apply -f manifests/30-payment-service-operator.yaml
kubectl -n otel-lab rollout status deployment/payment-service
```

---

## 4.4 驗證遷移成功

**(1) 手動 sidecar 不見了，`containers` 只剩 app：**

```bash
kubectl -n otel-lab get pod -l app=payment-service \
  -o jsonpath='{.items[0].spec.containers[*].name}{"\n"}'
# 遷移前：payment-service otel-collector-sidecar  ← 手動掛的，是普通 container
# 遷移後：payment-service                         ← 手動 sidecar 消失
```

**(2) Operator 注入的東西都在 `initContainers`（native sidecar + python agent）：**

跟 Stage 3 一樣，注入的 collector 是 native sidecar（`restartPolicy: Always` 的 initContainer）：

```bash
kubectl -n otel-lab get pod -l app=payment-service \
  -o jsonpath='{range .items[0].spec.initContainers[*]}{.name}({.restartPolicy}){" "}{end}{"\n"}'
# 預期：otc-container(Always) opentelemetry-auto-instrumentation-python()
#   ↑ otc-container 是 native sidecar；python 那個是把 SDK 複製進共享 volume 的 init
```

**(3) app container 被注入 Python auto-instrument 的 env：**

```bash
kubectl -n otel-lab get pod -l app=payment-service -o yaml \
  | grep -E 'PYTHONPATH|OTEL_SERVICE_NAME|OTEL_EXPORTER_OTLP_PROTOCOL|OTEL_TRACES_EXPORTER' -A1 | head
# 預期：
#   PYTHONPATH: /otel-auto-instrumentation-python/... ← agent 透過 sitecustomize 自動載入
#   OTEL_SERVICE_NAME: payment-service
#   OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf  ← Python 注入預設用 http/protobuf
```

> **背景知識展開（classroom 第 6 章）：為什麼 Python 注入方式跟 Java 完全不同？**
>
> Operator 對每個語言都有專屬的注入邏輯（[`internal/instrumentation/`](../../internal/instrumentation/) 下各一個檔案），但共同模式都是「initContainer 把 agent/SDK 複製進共享 emptyDir → 主 container 掛載該 volume → 用一個環境變數讓 runtime 在啟動時自動載入」：
>
> | 語言 | initContainer 動作 | 讓 runtime 載入的 env var | 檔案 |
> |---|---|---|---|
> | Java | `cp javaagent.jar` | `JAVA_TOOL_OPTIONS=-javaagent:...` | `javaagent.go` |
> | Python | `cp -r autoinstrumentation/.` | `PYTHONPATH=...`（直譯器啟動時載入 `sitecustomize.py`） | `python.go` |
> | Node.js | `cp -r autoinstrumentation/.` | `NODE_OPTIONS=--require ...` | `nodejs.go` |
> | Go（例外） | 不是 initContainer，是**額外的 sidecar container**，用 eBPF 追蹤主 process | 無（改用 `otel-go-auto-target-exe` 指定要 hook 的執行檔） | `golang.go` |
>
> Python 用 `PYTHONPATH` 而非「agent」的原因：Python 直譯器沒有像 JVM 那樣的 `-javaagent` 掛載機制，但可以透過 `sitecustomize.py`（Python 啟動時會自動 import 的模組）在 import 使用者程式碼之前，先把 instrumentation library 掛上去，效果等同 Java 的 agent。
>
> Operator 也會把 `OTEL_EXPORTER_OTLP_PROTOCOL` 設成 `http/protobuf`（因為官方 Python autoinstrumentation 走這個協議）——這正是我們把 sidecar 設成同時開 4317/4318、Instrumentation endpoint 指向 `:4318` 的原因。

---

## 4.5 打流量，確認端到端仍然正常且 trace 完整

```bash
curl -s -X POST "http://localhost:5000/pay?item=lamp&amount=5" | tee /dev/stderr
# 仍然回傳 payment_id 與「非空的」order，功能不變
```

> **看 gateway log 的正確方式（沿用 Stage 3 的 `gwlogs` 函式）：** gateway 有 2 個副本、debug 輸出量很大，而且 `kubectl logs -l` 會被預設 `--tail=10` 截斷。所以要「兩個 pod 都用名稱讀」：
> ```bash
> gwlogs() {  # 用法：gwlogs 25s
>   for p in $(kubectl -n otel-lab get pods -l app.kubernetes.io/instance=otel-lab.gateway -o name); do
>     kubectl -n otel-lab logs "$p" --since="${1:-20s}" 2>/dev/null
>   done
> }
> ```

payment 端現在是注入的 sidecar（`otc-container`），它把資料轉送到 agent，自己不印 span；要在 **gateway** 看。打流量後等一下（tail_sampling decision_wait=10s）再看：

```bash
curl -s -X POST "http://localhost:5000/pay?item=lamp&amount=5" >/dev/null; sleep 14

# gateway 端應同時看到 payment-service 與 order-service（同一條 trace 的兩段）
gwlogs 25s | grep -E 'Name +: (POST /pay|POST /orders|INSERT labdb)' | sort | uniq -c
# 預期：POST /pay、POST /orders、INSERT labdb.order_record 都出現
```

**確認自訂 metrics / log 活過了遷移**（這是本階段的重點驗證）——它們現在應該出現在 gateway，而且不再依賴 app 手動設定的 SDK：

```bash
curl -s -X POST "http://localhost:5000/pay?item=verify&amount=9" >/dev/null; sleep 12

# 自訂 metrics：payments.count / payments.amount 仍在（改由 agent 提供的 MeterProvider 匯出）
gwlogs 25s | grep -oE 'payments\.(count|amount)' | sort | uniq -c

# 自訂 log：'payment processed' 仍在（改由注入的 logging handler 匯出）
gwlogs 25s | grep 'payment processed' | head
```

> 業務 code 一行沒改，自訂 metrics/log 卻無縫接到了新的管線——這就是 4.2.1「留 API、去 SDK」設計的回報。

至此，**整條 trace（payment → order → postgres）以及自訂 metrics/log，全部由 Operator 管理的管線收集，經 agent 的 loadbalancing 收斂、送進 gateway 交給 tail_sampling 決策（error/slow 全留、其餘只隨機留 10%——metrics/log 不經過這個 processor，不受影響）。**

> 因為 tail_sampling 只留一部分「又快又成功」的 trace，實際跑這份 lab 打單一筆流量，有很高機率在 gateway log 看不到對應的 trace（這是預期行為，不是管線壞了）。要穩定看到結果，建議連續打十幾二十筆流量再檢查。

---

## 4.6 遷移帶來的維運差異

```
改一個 exporter / sampling 參數：

  遷移前：改 03-payment-service.yaml 裡的 ConfigMap → 重新部署 payment app
         （每個服務各一份，N 個服務改 N 次）

  遷移後：改 app-sidecar / gateway CR 一處 → Operator reconcile 所有受影響的 collector
         （Stage 5 還能從遠端 OpAMP server 下推，連 kubectl apply 都省了）
```

---

## 練習 4

**動手題（重要）：** 故意「同時」開手動與自動——在 `30-payment-service-operator.yaml` 把 `MANUAL_OTEL=true` 加回去（保留 annotation），重新 apply，打一筆 `/pay`，觀察 sidecar log 裡 span 是否出現重複或異常。觀察完把它移除。

**閱讀理解題：** 遷移後 app.py 裡其實還留著 `_init_manual_otel()` 函式（只是沒被呼叫）和 requirements 裡的 `opentelemetry-*`。為什麼說「真實專案應該把它們刪掉」，留著有什麼壞處？

<details>
<summary>參考答案</summary>

**雙重 instrumentation 現象：** 同時 `MANUAL_OTEL=true` + auto-inject，會有兩套 SDK 初始化。常見症狀：同一個操作出現重複 span、TracerProvider 被覆蓋導致部分 span 遺失、或 exporter 指向不一致。實際行為依載入順序而定，總之是「不可預期」，這就是務必擇一的原因。

**留著的壞處：**
1. **誤觸風險**：哪天有人把 `MANUAL_OTEL` 設回 true，或 auto-inject 失效時手動 code 又悄悄生效，行為變得難以推理。
2. **相依負擔**：requirements 裡的 `opentelemetry-*` 版本可能跟 Operator 注入的 autoinstrumentation 版本不一致，造成混淆與潛在衝突。
3. **誤導讀者**：app code 裡留著 OTel 初始化，會讓人以為「可觀測性是 app 的責任」，違背 auto-instrumentation「基礎設施負責、app 無感」的設計意圖。

所以遷移完成的標誌是：app code 與 requirements 裡**完全沒有** OTel 的痕跡。
</details>

---

| | |
|---|---|
| 上一步 | [← Stage 3](./03-java-sidecar-and-autoinstrument.md) |
| 下一步 | [Stage 5：用 OpAMP Bridge 當控制面 →](./05-opamp-bridge-control-plane.md) |
