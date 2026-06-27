# Stage 1：重現「接手時的現況」

> **本階段目標：** 部署 PostgreSQL + Java order-service（無 OTel）+ Python payment-service（手動 OTel + 自掛 sidecar collector），跑出一條跨服務呼叫，親眼確認「Java 沒有 telemetry、Python 各管各的」這個起點。

---

## 1.1 部署三個元件

```bash
cd classroom/lab

kubectl apply -f manifests/00-namespace.yaml
kubectl apply -f manifests/01-postgres.yaml
kubectl apply -f manifests/02-order-service.yaml      # Java，無 OTel
kubectl apply -f manifests/03-payment-service.yaml    # Python，手動 OTel + 自掛 sidecar collector

kubectl -n otel-lab rollout status deployment/postgres
kubectl -n otel-lab rollout status deployment/order-service
kubectl -n otel-lab rollout status deployment/payment-service
```

---

## 1.2 觀察「現況」的兩個關鍵差異

**Java order-service：只有 1 個 container，沒有任何 OTel。**

```bash
kubectl -n otel-lab get pod -l app=order-service \
  -o jsonpath='{.items[0].spec.containers[*].name}{"\n"}'
# 輸出：order-service
```

**Python payment-service：有 2 個 container —— app 自己 + 手動掛的 sidecar collector。**

```bash
kubectl -n otel-lab get pod -l app=payment-service \
  -o jsonpath='{.items[0].spec.containers[*].name}{"\n"}'
# 輸出：payment-service otel-collector-sidecar
```

這正是情境圖：

```
order-service Pod              payment-service Pod
┌────────────────┐            ┌─────────────────────────────┐
│ order-service  │            │ payment-service             │
│ (Java, 無 OTel) │            │ (Python, 手動 OTel SDK)      │
└────────────────┘            │   │ localhost:4317            │
                              │   ▼                          │
                              │ otel-collector-sidecar       │
                              │ (手動掛, 設定寫死在 manifest)  │
                              └─────────────────────────────┘
```

---

## 1.3 打一筆流量，產生一條跨服務呼叫

```bash
# 把 payment-service 轉發到本機
kubectl -n otel-lab port-forward svc/payment-service 5000:5000 &

# 打一筆付款（payment → order → postgres）
curl -s -X POST "http://localhost:5000/pay?item=book&amount=3" | tee /dev/stderr
# 預期回傳：{"order":{"amount":3,"id":1,"item":"book",...},"payment_id":1}
```

---

## 1.4 看看現在能觀測到什麼、不能觀測到什麼

**Python 端：手動 sidecar collector 收到三種訊號（trace + 自訂 metrics + 自訂 log）。**

payment-service 不只有 trace，app code 還主動寫了**自訂業務遙測**：
- metrics：`payments.count`（counter）、`payments.amount`（histogram）
- log：每筆付款一行結構化 log

先打一筆流量，再用 `--since` 抓最近幾秒（collector 的 debug 輸出會被 health-check log 持續洗掉，用 `--since` 比 `--tail` 可靠）：

```bash
curl -s -X POST "http://localhost:5000/pay?item=signal&amount=1" >/dev/null
sleep 6   # 自訂 metrics 預設每 5s 匯出一次

# trace：/pay 的 HTTP server span、requests client span、psycopg2 span
kubectl -n otel-lab logs -l app=payment-service -c otel-collector-sidecar --since=15s | grep -cE 'Span #'

# 自訂 metrics：找得到 payments.count / payments.amount
kubectl -n otel-lab logs -l app=payment-service -c otel-collector-sidecar --since=15s | grep -oE 'payments\.(count|amount)' | sort | uniq -c

# 自訂 log：找得到 "payment processed" 這行 log record
kubectl -n otel-lab logs -l app=payment-service -c otel-collector-sidecar --since=15s | grep 'payment processed' | head
```

> 這三種訊號都是 app **自己**產生的：trace 來自 instrumentation library，metrics/log 來自 app code 主動寫（`app.py` 裡的 `payments_counter` / `payment_amount` / `log.info(...)`）。Stage 4 遷移時，這些自訂 metrics/log 能不能保住，是遷移成敗的關鍵——先記住這點。

**Java 端：完全沒有 telemetry。**

```bash
# order-service 根本沒有 collector 容器可看，也沒有任何 span 被產生
kubectl -n otel-lab logs -l app=order-service --tail=20
# 只有 Spring Boot 自己的 log，沒有任何 OTel / span 字樣
```

---

## 1.5 現況的痛點（這就是後面要解決的）

```
┌──────────────────────────────────────────────────────────────┐
│ 痛點 1：trace 斷掉                                              │
│   payment 的 span 看得到「呼叫了 order-service」，               │
│   但 order-service 內部（Spring MVC、JDBC）完全沒有 span，       │
│   trace 在跨服務邊界就斷了。                                     │
│                                                                │
│ 痛點 2：collector 設定散落                                       │
│   payment 的 collector 設定寫死在 03-payment-service.yaml 的     │
│   ConfigMap，要改 sampling / exporter 就得改 app 的 manifest。   │
│                                                                │
│ 痛點 3：沒有集中匯流與 tail sampling                            │
│   每個服務各送各的，沒有統一的 gateway 做全域取樣決策。            │
└──────────────────────────────────────────────────────────────┘
```

接下來四個階段逐一解決：

| 痛點 | 由哪一階段解決 |
|---|---|
| 沒有集中匯流 + tail sampling + span 負載平衡 | Stage 2 |
| Java 沒有 telemetry | Stage 3 |
| Python collector 各管各的 | Stage 4 |
| 缺集中管理控制面 | Stage 5 |

---

## 練習 1

**動手題：** 把 `03-payment-service.yaml` 裡 collector 的 exporter 從 `debug` 改成同時輸出到一個檔案（提示：加 `verbosity: basic` 觀察差異），重新 apply，再打一次 `/pay`，比較 log 量。

**思考題：** 痛點 2 說「要改 sampling 就得改 app manifest」。如果有 50 個服務都這樣手動掛 collector，改一個 exporter 參數要動幾個檔案？Stage 2~4 之後會變成動幾個？

<details>
<summary>參考答案</summary>

現況：50 個服務 = 50 份 collector ConfigMap（散落在各 app manifest），改 exporter 要動 50 個檔案、重新部署 50 個 app。

用 Operator 後：app 只留 annotation，collector 設定集中在少數幾個 `OpenTelemetryCollector` CR（gateway / agent / sidecar 範本各一）。改 exporter 只要改 CR，Operator 會自動 reconcile 對應的 Deployment/DaemonSet/ConfigMap。Stage 5 再加上 OpAMP，連「改 CR」都能從一個遠端 server 下推。
</details>

---

| | |
|---|---|
| 上一步 | [← Stage 0](./00-setup.md) |
| 下一步 | [Stage 2：Gateway Collector + Span Load Balancer →](./02-collector-gateway-and-loadbalancer.md) |
