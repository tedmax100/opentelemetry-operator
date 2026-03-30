# Module 6：OpAMP Bridge

## 目標

理解 OpAMP 協議的用途，部署 OpAMP server 和 OpAMPBridge CR，並實際體驗在不重新部署的情況下，遠端動態更新 collector 的 pipeline 設定。

---

## 背景：OpAMP 是什麼

**OpAMP（Open Agent Management Protocol）** 是 OpenTelemetry 定義的 agent 遠端管理協議，讓 management server 能夠：

- 遠端更新 agent/collector 的 config（不需要 kubectl apply）
- 監控 agent 的健康狀態
- 查詢 agent 目前的 config
- 在大規模環境下統一管理幾百個 collector

```
傳統方式：
  修改 config → kubectl apply → collector rollout → 等待重啟

OpAMP 方式：
  在 OpAMP server 修改 config → collector 收到通知 → 熱更新（部分情況免重啟）
```

---

## 架構

```
                OpAMP Server（管理介面）
                      │
                   WebSocket
                      │
                OpAMPBridge（CRD）
                      │
           ┌──────────┼──────────┐
           ▼          ▼          ▼
      Collector-1  Collector-2  Collector-3
```

OpAMPBridge 的角色：
- 作為 OTel collector 和 OpAMP server 之間的橋樑
- 把 OpAMP server 下發的 config 轉換為 `OpenTelemetryCollector` CR 更新
- 向 OpAMP server 回報 collector 狀態

---

## 步驟

### 6.1 部署輕量 OpAMP Server

這裡使用 [opamp-go](https://github.com/open-telemetry/opamp-go) 的範例 server，適合學習用：

```bash
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: opamp-server
  namespace: observability
spec:
  replicas: 1
  selector:
    matchLabels:
      app: opamp-server
  template:
    metadata:
      labels:
        app: opamp-server
    spec:
      containers:
        - name: opamp-server
          image: ghcr.io/open-telemetry/opamp-go/opampsrv:latest
          ports:
            - containerPort: 4320   # OpAMP WebSocket
            - containerPort: 4321   # HTTP 管理 API
---
apiVersion: v1
kind: Service
metadata:
  name: opamp-server
  namespace: observability
spec:
  selector:
    app: opamp-server
  ports:
    - name: opamp
      port: 4320
      targetPort: 4320
    - name: api
      port: 4321
      targetPort: 4321
EOF
```

---

### 6.2 建立 OpAMPBridge CR

```bash
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1alpha1
kind: OpAMPBridge
metadata:
  name: demo-bridge
  namespace: observability
spec:
  # 連接 OpAMP server
  endpoint: ws://opamp-server.observability:4320/v1/opamp

  # 這個 bridge 管理哪些 collector
  # 使用 label selector
  componentsAllowed:
    receivers:
      - otlp
      - prometheus
      - hostmetrics
      - kubeletstats
    processors:
      - batch
      - memory_limiter
      - k8sattributes
      - resource
      - filter
    exporters:
      - otlp
      - otlp/tempo
      - prometheusremotewrite
      - debug

  capabilities:
    # Bridge 告訴 OpAMP server 它支援哪些功能
    reportEffectiveConfig: true      # 回報目前生效的 config
    acceptRemoteConfig: true         # 接受遠端下發的 config
    reportOwnMetrics: true           # 回報自身 metrics
    reportOwnLogs: true              # 回報自身 logs
    acceptPackages: false            # 不接受 package 安裝（安全考量）

  serviceAccount: otel-bridge
EOF
```

---

### 6.3 設定 Bridge 的 RBAC

Bridge 需要能讀寫 `OpenTelemetryCollector` CR：

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: otel-bridge
  namespace: observability
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: otel-bridge
  namespace: observability
rules:
  - apiGroups: ["opentelemetry.io"]
    resources: ["opentelemetrycollectors"]
    verbs: ["get", "list", "watch", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: otel-bridge
  namespace: observability
subjects:
  - kind: ServiceAccount
    name: otel-bridge
    namespace: observability
roleRef:
  kind: Role
  name: otel-bridge
  apiGroup: rbac.authorization.k8s.io
EOF
```

---

### 6.4 確認 Bridge 連線成功

```bash
kubectl -n observability logs -l app.kubernetes.io/name=demo-bridge --tail=20
# 應看到：
# "Connected to OpAMP Server"
# "Sending AgentToServer message"
```

查看 OpAMP server 收到的 agents：

```bash
kubectl -n observability port-forward svc/opamp-server 4321:4321 &
curl http://localhost:4321/agents | jq .
# 應看到 demo-bridge 的資訊，包含目前的 collector config
```

---

### 6.5 透過 OpAMP 動態更新 Collector Config

這是 OpAMP 的核心功能：不需要 kubectl，直接在 server 端推送新 config。

透過 OpAMP server 的 API 更新 otel-gateway 的 config，加入一個 filter processor（過濾掉 health check span）：

```bash
# 讀取目前的 collector config（Base64 encoded）
CURRENT_CONFIG=$(curl -s http://localhost:4321/agents | jq -r '.[0].effectiveConfig' | base64 -d)
echo "$CURRENT_CONFIG"
```

推送新 config：

```bash
NEW_CONFIG=$(cat <<'EOF'
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 400
    spike_limit_mib: 100

  # 新增：過濾掉 health check 的 span
  filter/drop-healthcheck:
    error_mode: ignore
    traces:
      span:
        - 'attributes["http.target"] == "/health"'
        - 'attributes["http.url"] endswith "/health"'

  k8sattributes:
    auth_type: serviceAccount
    extract:
      metadata:
        - k8s.pod.name
        - k8s.namespace.name
        - k8s.deployment.name

  batch:
    timeout: 5s

exporters:
  otlp/tempo:
    endpoint: tempo.observability:4317
    tls:
      insecure: true
  prometheusremotewrite:
    endpoint: http://prometheus-server.observability/api/v1/write

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, filter/drop-healthcheck, k8sattributes, batch]
      exporters: [otlp/tempo]
    metrics:
      receivers: [otlp]
      processors: [memory_limiter, k8sattributes, batch]
      exporters: [prometheusremotewrite]
EOF
)

# 把新 config 推給 OpAMP server
curl -X POST http://localhost:4321/agents/config \
  -H "Content-Type: application/json" \
  -d "{\"config\": \"$(echo "$NEW_CONFIG" | base64 -w0)\"}"
```

---

### 6.6 觀察 Config 更新效果

```bash
# 觀察 bridge log
kubectl -n observability logs -l app.kubernetes.io/name=demo-bridge -f
# 應看到：
# "Received new config from OpAMP Server"
# "Updating OpenTelemetryCollector CR"

# 觀察 collector 是否更新（可能 rollout，也可能熱更新）
kubectl -n observability get pod -l app.kubernetes.io/name=otel-gateway-collector -w
```

更新後，發幾個 health check 請求：

```bash
for i in {1..5}; do
  curl -s http://localhost:8000/health
  curl -s -X POST http://localhost:8000/orders \
    -H "Content-Type: application/json" \
    -d '{"product_id": "P001", "quantity": 1}'
done
```

在 Tempo 確認：`/health` 的 trace 被過濾掉，只看到 `/orders` 的 trace。

---

## OpAMP vs kubectl apply 的適用場景

| 場景 | 建議方式 |
|------|---------|
| 開發/測試環境 | kubectl apply（直接改 YAML 最快）|
| 小型生產環境 | kubectl apply（GitOps 管理）|
| 大規模部署（幾十到幾百個 collector）| OpAMP（統一管理，不需要逐一 apply）|
| 需要即時緊急調整（如臨時提高 sampling rate）| OpAMP（最快生效）|
| 多租戶環境 | OpAMP（可以按 agent 分別下發 config）|

---

## 完成檢查點

- [ ] OpAMP server pod 正常運行
- [ ] OpAMPBridge pod 已連上 OpAMP server
- [ ] OpAMP server API 能看到 bridge 的狀態
- [ ] 透過 OpAMP API 推送新 config 後，collector 行為改變
- [ ] `/health` 的 trace 在 Tempo 中消失

---

## 課程總結

恭喜完成所有模組！你現在已經實際操作過：

| 功能 | 對應模組 |
|------|---------|
| Python/Java auto-instrumentation，零程式碼改動 | Module 1 |
| Collector 集中 gateway，pipeline 設計 | Module 2 |
| Node-level host metrics，DaemonSet agent | Module 3 |
| Per-pod sidecar collector，完全隔離 | Module 4 |
| TargetAllocator，Prometheus scrape 負載分配 | Module 5 |
| OpAMP，遠端動態管理 collector config | Module 6 |

整套架構的完整流向：

```
應用程式（Python/Java）
    │ auto-instrumentation（Module 1）
    ▼
Sidecar Collector（Module 4）
    │ forward
    ▼
Gateway Collector（Module 2）  ←── TargetAllocator（Module 5）scrape demo services
    │ 由 OpAMP Bridge（Module 6）遠端管理 config
    ├──► Grafana Tempo（traces）
    └──► Prometheus（metrics）
         │
         ▼
      Grafana Dashboard

Node Agent（Module 3）
    │ host metrics
    └──► Gateway Collector（Module 2）
```
