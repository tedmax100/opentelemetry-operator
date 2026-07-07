# Stage 8（延伸）：接上 Grafana + Tempo + Prometheus——看 signal 真的出現

> **本階段目標：** 把 gateway 的出口從 debug exporter 換成真實後端（Tempo 收 traces、Prometheus 收 metrics、Grafana 看），然後親眼驗證這個 lab 的核心命題：**一個本來沒有 OTel 的服務（Stage 1 的 order-service），加上兩行 annotation 之後，trace 和 metrics 出現在 Grafana。**
>
> **前置：** 完成 Stage 0–3（Stage 4–7 可選；只要 order-service 是「已注入」狀態即可）。
> **預期時間：** 20–30 分鐘

---

## 8.1 為什麼要這個 stage

前面所有 stage 的驗證方式都是「看 gateway collector 的 log」——debug exporter 把 span/metric 印成文字。這對理解機制夠了，但有兩件事它給不了：

1. **signal 的真實樣貌**：跨服務的 trace 樹（payment → order → JDBC）、Java agent 自動吐的 JVM metrics，在 UI 上看跟在 log 裡看是兩回事。做分享 demo 時，「Grafana 上出現 trace」比「log 裡有一行字」有說服力得多。
2. **政策的可見證據**：Stage 2 設的 tail sampling（error/slow 全留、其餘 10%）在 debug log 裡感受不到，在 Tempo 的搜尋結果裡一目了然。

部署拓撲（全部在 `otel-lab` namespace，lab 級最小部署）：

```
sidecar → agent（load_balancing）→ gateway ──┬── otlp/tempo ──────▶ Tempo ──┐
                                             ├── otlphttp/prometheus ▶ Prometheus ──┤──▶ Grafana
                                             └── debug（保留，log 驗證照用）        │
                        gateway 自身 metrics（8888）◀── Prometheus scrape ──────────┘
```

兩個後端的接法刻意選了兩種不同機制，各代表一類真實場景：

| Signal | Exporter | 機制 |
|---|---|---|
| traces | `otlp/tempo`（gRPC 4317） | Tempo 原生收 OTLP，最單純 |
| metrics | `otlphttp/prometheus` | Prometheus 3.x 的**原生 OTLP ingestion**（`/api/v1/otlp`，需開 `--web.enable-otlp-receiver`）——push 模型，不用 prometheus exporter + scrape 那套 |

---

## 8.2 部署後端

```bash
kubectl apply -f manifests/70-observability-backend.yaml

# 等三個都 Ready
kubectl -n otel-lab rollout status deploy/tempo deploy/prometheus deploy/grafana
```

內容物（細節見 [manifest 內的註解](./manifests/70-observability-backend.yaml)）：

| 元件 | Image | 要點 |
|---|---|---|
| Tempo | `grafana/tempo:2.7.2` | 單體模式、local 儲存、block 只留 1h |
| Prometheus | `prom/prometheus:v3.5.0` | `--web.enable-otlp-receiver` 開 OTLP 端點；`out_of_order_time_window: 30m` 容忍亂序；`promote_resource_attributes` 把 `k8s.*` 升成 label |
| Grafana | `grafana/grafana:12.0.2` | 匿名 Admin 免登入（lab 專用）；datasource 用 provisioning ConfigMap 預先配好 |

---

## 8.3 改 gateway CR，資料出口改道

```bash
kubectl apply -f manifests/71-collector-gateway-backends.yaml

# 觀察 Operator 滾動更新 StatefulSet
kubectl -n otel-lab rollout status statefulset/gateway-collector
```

停在這裡體會一下治理語意：這份 YAML 的 CR 名稱同樣是 `gateway`，所以 apply = 改既有 CR 的 `spec.config`。Operator 偵測到差異，重建 ConfigMap、滾動更新 StatefulSet——**全公司 telemetry 的出口從 debug 改道到 Tempo/Prometheus，業務團隊的 Deployment、sidecar CR、agent CR 一個字都沒動**。這就是「後端地址只在 gateway 設一次」的兌現：哪天要換後端（Tempo → 別家），也是同一個動作。

debug exporter 刻意保留在 pipeline 裡，前面各 stage「看 gateway log」的驗證方式照常可用。

---

## 8.4 產生流量，在 Grafana 看 signal

```bash
# Grafana UI
kubectl -n otel-lab port-forward svc/grafana 3000:3000 &
# 流量入口（若 Stage 1 的 port-forward 還在就略過）
kubectl -n otel-lab port-forward svc/payment-service 5000:5000 &

# 正常流量：30 筆，每筆走 payment → order → PostgreSQL
for i in $(seq 1 30); do
  curl -s -X POST "http://localhost:5000/pay?item=book&amount=$i" >/dev/null
  sleep 1
done

# error 流量：amount 給非數字，payment-service 的 int() 會炸 500
curl -s -X POST "http://localhost:5000/pay?item=explode&amount=abc"
```

打開 http://localhost:3000 → Explore：

**看 traces（datasource 選 Tempo）**：Search → Service Name 選 `order-service` → 應該看到 trace 樹：

```
payment-service  POST /pay
  └─ order-service  POST /orders          ← Stage 3 注入的 Java agent 產生
       └─ INSERT labdb.orders (JDBC)      ← 連 SQL span 都有，zero code
```

兩個必看的現象：

- **正常 trace 只出現大約 10%**（30 筆大概看到 3–4 條）。這不是掉資料，是 Stage 2 的 tail sampling 政策在生效——第一次有了政策生效的**視覺證據**。
- 那筆 `amount=abc` 的 **error trace 100% 出現**（error policy 全留），紅色 status 一眼可見。

**看 metrics（datasource 選 Prometheus）**：

```promql
# Java agent 自動吐的 JVM metrics（OTLP 命名 jvm.memory.used → Prom 命名加單位後綴）
jvm_memory_used_bytes{job="otel-lab/order-service"}

# HTTP server 延遲直方圖的請求數
rate(http_server_request_duration_seconds_count{job="otel-lab/order-service"}[1m])
```

兩個命名翻譯規則，第一次看會困惑，先講明：

1. **`job` label = `<service.namespace>/<service.name>`**。operator 注入時自動把 Pod 的 k8s namespace 設成 `service.namespace`，所以是 `otel-lab/order-service` 而不是 `order-service`。
2. **OTLP metric 名進 Prometheus 會翻譯**：`.` 換 `_`、按單位補後綴（`jvm.memory.used`（By）→ `jvm_memory_used_bytes`；duration（s）的 histogram → `_seconds_bucket/_count/_sum`）。

---

## 8.5 本階段的重頭戲：before / after 重播

這就是你想看的「本來沒有 OTel 的服務，加入後 signal 出現」——反過來演一次更有力：

```bash
# 1) 把 order-service 退回 Stage 1 的「無 OTel」版本（唯一差別：沒有那三行 annotation）
kubectl apply -f manifests/02-order-service.yaml
kubectl -n otel-lab rollout status deploy/order-service

# 2) 再打流量
for i in $(seq 1 10); do curl -s -X POST "http://localhost:5000/pay?item=probe&amount=1" >/dev/null; sleep 1; done
```

Grafana 上的變化：

- **Tempo**：trace 斷鏈——只剩 `payment-service POST /pay` 一個孤立 span，`order-service` 和 JDBC span 消失（order-service 收到了帶 traceparent 的請求，但沒有 agent 去續這條 trace）。
- **Prometheus**：`jvm_memory_used_bytes{job="otel-lab/order-service"}` 的線停止更新。

```bash
# 3) 加回 annotation（Stage 3 版；做過 Stage 7 的話用 51 也一樣）
kubectl apply -f manifests/21-order-service-instrumented.yaml
kubectl -n otel-lab rollout status deploy/order-service
# 再打流量 → trace 樹恢復完整、JVM metrics 的線重新開始走
```

一來一回，「**annotation = signal 的開關，app image 全程沒變**」不再是一句主張，是眼前發生的事。分享時這段 3 分鐘就演完，是全場最有說服力的 demo。

---

## 驗證清單

| 檢查 | 指令 / 位置 | 預期 |
|---|---|---|
| 三個後端 Ready | `kubectl -n otel-lab get deploy tempo prometheus grafana` | 各 1/1 |
| gateway 滾動完成 | `kubectl -n otel-lab rollout status statefulset/gateway-collector` | 2 個副本 Ready |
| Tempo 有 trace | Grafana Explore → Tempo → Search `order-service` | 完整 trace 樹（含 JDBC span） |
| error 全留 | 打一筆 `amount=abc` | 該 trace 必出現且標紅 |
| Prometheus 有 OTLP metrics | `jvm_memory_used_bytes{job="otel-lab/order-service"}` | 有資料 |
| gateway 自身 metrics | Prometheus 查 `otelcol_exporter_sent_spans` | 有資料（scrape 8888 進來的） |

---

## 練習 8

**1.（閱讀理解）** 為什麼 traces 用 `otlp`（gRPC）exporter、metrics 卻要用 `otlphttp`？把兩個後端的「收 OTLP 的方式」各用一句話說出來。

<details><summary>參考答案</summary>

Tempo 原生支援 OTLP gRPC（4317）與 HTTP（4318），選 gRPC 是慣例。Prometheus 的 OTLP ingestion **只有 HTTP 端點**（`/api/v1/otlp/v1/metrics`，`otlphttp` exporter 會自動在 endpoint 後補 `/v1/metrics`），且必須以 `--web.enable-otlp-receiver` 明確開啟——它是 push 進 TSDB，不是 scrape。所以 exporter 型別由**後端的協議支援**決定，不是偏好。
</details>

**2.（動手）** 把 `71` 裡 tail sampling 的 `sampling_percentage` 從 10 改成 100 再 apply，重打 30 筆正常流量，Tempo 的搜尋結果有什麼變化？這個「全公司採樣率變更」總共改了幾個檔案、動了幾個團隊的部署？

<details><summary>參考答案</summary>

30 筆 trace 全部出現在 Tempo。改動 = 一份 CR 的一個數字，`kubectl apply` 後 Operator 滾動更新 gateway；業務團隊的部署零變動。對照「沒有 Operator 的世界」：這個變更要動每一個有 collector 設定的 repo。這就是教材說「sampling 政策只存在一個地方」的具體體感。
</details>

**3.（思考）** logs pipeline 還停在 debug exporter。要把 logs 接到 Loki，你會怎麼做？需要動到 sidecar 或 agent 嗎？

<details><summary>參考答案</summary>

跟 8.3 同一招：部署 Loki（收 OTLP 的話開它的 `/otlp` ingestion 端點），在 gateway CR 加一個 `otlphttp/loki` exporter、把 logs pipeline 的 exporter 換掉，apply。sidecar/agent 完全不用動——它們只負責把三種 signal 往上送，不知道也不需要知道後端是誰。另外記得把新 exporter type 加進 OpAMPBridge 的 `componentsAllowed`（Stage 8 已為 `otlphttp` 加過，見 `40-opampbridge.yaml` 的註解）。
</details>

**4.（思考）** 8.5 的 before/after 裡，退回無 OTel 版本後，payment-service 的 trace 為什麼還在，而且看不到任何錯誤？這說明 trace context 傳播和 instrumentation 是兩件獨立的事——分別由誰負責？

<details><summary>參考答案</summary>

payment-service 自己的 instrumentation（Stage 4 注入）還在，所以它的 span 照常產生與上報；它也照常把 `traceparent` header 送給 order-service——但 order-service 沒有 agent，收到 header 也沒有東西去建立 child span、更不會續傳給 JDBC。所以 trace「斷」在服務邊界，但不是錯誤：**context 傳播由呼叫方的 instrumentation 放進 header，續鏈由被呼叫方的 instrumentation 負責**——覆蓋率缺一個服務，斷的是整條鏈的可見性，這正是「擴散要做到全覆蓋」的理由。
</details>

---

## 清理（可選）

```bash
kubectl apply -f manifests/10-collector-gateway.yaml     # gateway 退回 debug-only
kubectl delete -f manifests/70-observability-backend.yaml
```

| | |
|---|---|
| 上一步 | [← Stage 7：業務 attributes 與 CR 隔離](./07-team-scoped-attributes.md) |
| 回目錄 | [README](./README.md) |
