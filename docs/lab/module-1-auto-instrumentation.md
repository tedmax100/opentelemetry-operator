# Module 1：Auto-Instrumentation

## 目標

不修改任何應用程式程式碼，透過 annotation 讓 Python 和 Java 服務自動產生 traces，並在 Grafana Tempo 看到完整的 distributed trace。

---

## 背景：機制回顧

OTel operator 的 auto-instrumentation 利用三個 Kubernetes 原生機制：

1. **Mutating Webhook**：在 Pod 創建前攔截，修改 Pod spec
2. **Init Container**：從預建的 instrumentation image 複製函式庫到 EmptyDir volume
3. **環境變數注入**：設定 `PYTHONPATH`（Python）或 `-javaagent`（Java），讓 runtime 自動載入 OTel SDK

```
Pod 啟動順序：
  init container（cp 函式庫到 /otel-auto-instrumentation-python）
       ↓
  app container 啟動
       ↓
  Python import → sitecustomize.py → OTel SDK 初始化 → 自動 patch 所有支援的框架
```

---

## 步驟

### 1.1 建立 Instrumentation CR

`Instrumentation` CR 定義了：使用哪個 instrumentation image、trace 要送去哪、propagator 設定等。

```bash
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: demo-instrumentation
  namespace: demo
spec:
  # Trace 送往 Module 2 會建立的 collector，現在先直送 Tempo
  exporter:
    endpoint: http://tempo.observability:4317

  propagators:
    - tracecontext   # W3C TraceContext（主流標準）
    - baggage        # W3C Baggage（傳遞 metadata）
    - b3             # B3（相容 Zipkin）

  sampler:
    type: parentbased_traceidratio
    argument: "1"    # 100% 取樣，學習環境用

  python:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-python:latest
    env:
      - name: OTEL_LOG_LEVEL
        value: debug  # 學習時方便觀察

  java:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-java:latest
EOF
```

---

### 1.2 對 Python 服務加 annotation

```bash
kubectl patch deployment order-service -n demo \
  --type=json \
  -p='[
    {
      "op": "add",
      "path": "/spec/template/metadata/annotations",
      "value": {
        "instrumentation.opentelemetry.io/inject-python": "demo-instrumentation"
      }
    }
  ]'
```

或直接編輯 manifest：

```yaml
# order-service Deployment
spec:
  template:
    metadata:
      annotations:
        instrumentation.opentelemetry.io/inject-python: "demo-instrumentation"
```

---

### 1.3 觀察 Webhook 的效果

Patch 後，operator webhook 會修改 Pod spec。查看新啟動的 pod：

```bash
kubectl -n demo get pod -l app=order-service -o yaml | grep -A 30 initContainers
```

應看到 operator 注入的 init container：

```yaml
initContainers:
- name: opentelemetry-auto-instrumentation-python
  image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-python:latest
  command:
  - cp
  - -r
  - /autoinstrumentation/.          # 從 image 複製
  - /otel-auto-instrumentation-python  # 到 emptyDir volume
  volumeMounts:
  - mountPath: /otel-auto-instrumentation-python
    name: opentelemetry-auto-instrumentation-python
```

查看注入的環境變數：

```bash
kubectl -n demo get pod -l app=order-service -o jsonpath='{.items[0].spec.containers[0].env}' | jq .
```

應看到：

```json
[
  { "name": "PYTHONPATH", "value": "/otel-auto-instrumentation-python/opentelemetry/instrumentation/auto_instrumentation:/otel-auto-instrumentation-python" },
  { "name": "OTEL_SERVICE_NAME", "value": "order-service" },
  { "name": "OTEL_EXPORTER_OTLP_ENDPOINT", "value": "http://tempo.observability:4317" },
  { "name": "OTEL_EXPORTER_OTLP_PROTOCOL", "value": "http/protobuf" },
  { "name": "OTEL_TRACES_EXPORTER", "value": "otlp" },
  { "name": "OTEL_PROPAGATORS", "value": "tracecontext,baggage,b3" }
]
```

> **注意**：`OTEL_SERVICE_NAME` 由 operator 從 Pod label 或 Deployment name 自動推導，不需手動設定。

---

### 1.4 對 Java 服務加 annotation

```bash
kubectl patch deployment inventory-service -n demo \
  --type=json \
  -p='[
    {
      "op": "add",
      "path": "/spec/template/metadata/annotations",
      "value": {
        "instrumentation.opentelemetry.io/inject-java": "demo-instrumentation"
      }
    }
  ]'
```

Java 的機制與 Python 不同，觀察注入的環境變數：

```bash
kubectl -n demo get pod -l app=inventory-service -o jsonpath='{.items[0].spec.containers[0].env}' | jq .
```

Java 使用 `-javaagent` 而非 `PYTHONPATH`：

```json
{ "name": "JAVA_TOOL_OPTIONS", "value": "-javaagent:/otel-auto-instrumentation-java/javaagent.jar" }
```

---

### 1.5 產生 Trace

```bash
kubectl -n demo port-forward svc/order-service 8000:8000 &

# 建立幾筆訂單
for i in {1..5}; do
  curl -s -X POST http://localhost:8000/orders \
    -H "Content-Type: application/json" \
    -d "{\"product_id\": \"P00$i\", \"quantity\": $i}"
  sleep 1
done
```

---

### 1.6 在 Grafana Tempo 觀察 Trace

```bash
kubectl -n observability port-forward svc/grafana 3000:80 &
# 開啟 http://localhost:3000
```

1. 進入 **Explore** → 選 **Tempo** data source
2. 搜尋 `service.name = "order-service"`
3. 點開一個 trace，應看到：
   - `POST /orders`（FastAPI handler）
   - `INSERT INTO orders`（PostgreSQL span）
   - `publish order.created`（RabbitMQ publish span）
4. 切換到 `inventory-service`，應看到：
   - `consume order.created`（RabbitMQ consume span）
   - `UPDATE inventory`（PostgreSQL span）
5. 確認兩個 service 的 span 在同一個 trace 裡（透過 W3C TraceContext propagation）

---

## 進階練習

### 練習 A：只對特定 container 注入

當一個 Pod 有多個 container 時：

```yaml
annotations:
  instrumentation.opentelemetry.io/inject-python: "demo-instrumentation"
  instrumentation.opentelemetry.io/python-container-names: "app"  # 只注入 app container
```

### 練習 B：Alpine image（musl）

如果應用程式用 Alpine base image：

```yaml
annotations:
  instrumentation.opentelemetry.io/inject-python: "demo-instrumentation"
  instrumentation.opentelemetry.io/otel-python-platform: "musl"
```

Init container 會改為複製 `/autoinstrumentation-musl/.` 而非 `/autoinstrumentation/.`

### 練習 C：調整取樣率

修改 `Instrumentation` CR 的 sampler：

```yaml
sampler:
  type: parentbased_traceidratio
  argument: "0.1"  # 只取樣 10%
```

觀察 Tempo 的 trace 數量變化。

### 練習 D：加入自訂 resource attributes

```yaml
spec:
  env:
    - name: OTEL_RESOURCE_ATTRIBUTES
      value: "deployment.environment=lab,team=platform"
```

在 Tempo 的 trace 裡應看到這兩個 attribute。

---

## 完成檢查點

- [ ] order-service pod 有 init container `opentelemetry-auto-instrumentation-python`
- [ ] inventory-service pod 有 init container `opentelemetry-auto-instrumentation-java`
- [ ] Grafana Tempo 可看到 order-service 的 trace
- [ ] Grafana Tempo 可看到跨越 order-service → inventory-service 的 distributed trace
- [ ] Trace 包含 PostgreSQL 和 RabbitMQ 的 span

---

## 下一步

現在 trace 是直接從應用程式送到 Tempo，中間沒有 collector。這有幾個問題：
- 每個應用都要知道 Tempo 的地址
- 沒有辦法在中間做 batching、filtering、enrichment
- endpoint 變更要重新 rollout 所有服務

→ [Module 2：Collector — Deployment 模式](./module-2-collector-deployment.md) — 加入 collector 作為中間層
