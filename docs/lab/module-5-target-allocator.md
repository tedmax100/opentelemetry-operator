# Module 5：TargetAllocator

## 目標

理解 TargetAllocator 解決的問題，並實際觀察它如何在多個 collector 副本之間均勻分配 Prometheus scrape targets，避免重複抓取。

---

## 問題背景：Prometheus Scraping 的挑戰

假設 order-service 和 inventory-service 各自有 `/metrics` endpoint，而 collector 有 3 個副本：

```
沒有 TargetAllocator 的情況：

Collector replica-1  ──scrape──► order-service:8000/metrics  ✓
Collector replica-2  ──scrape──► order-service:8000/metrics  ✗ 重複！
Collector replica-3  ──scrape──► order-service:8000/metrics  ✗ 重複！

結果：Prometheus 裡每個 metric 有 3 份，數值錯誤
```

TargetAllocator 的解法：

```
有 TargetAllocator 的情況：

TargetAllocator 發現 3 個 targets：
  - order-service:8000/metrics
  - inventory-service:8080/metrics
  - postgres-exporter:9187/metrics

分配：
  Collector replica-1  ──scrape──► order-service:8000/metrics
  Collector replica-2  ──scrape──► inventory-service:8080/metrics
  Collector replica-3  ──scrape──► postgres-exporter:9187/metrics

結果：每個 target 只被 scrape 一次，負載均衡
```

---

## 架構

```
ServiceMonitor / PodMonitor（或靜態 scrape config）
        │
        ▼
  TargetAllocator  ←── 監控 k8s API，發現 targets
        │
        │  分配 targets 給各 collector
        │  （HTTP API: /jobs/{job}/targets）
        ▼
  Collector replica-1  ──scrape──► target A
  Collector replica-2  ──scrape──► target B
  Collector replica-3  ──scrape──► target C
```

---

## 步驟

### 5.1 讓 Demo 服務暴露 Metrics

**order-service** 需要加入 Prometheus metrics（如果使用 opentelemetry-sdk，指標會透過 OTLP 送出，但 Prometheus scrape 需要另外的 endpoint）。

```bash
# 確認 order-service 有 /metrics endpoint
kubectl -n demo port-forward svc/order-service 8000:8000 &
curl http://localhost:8000/metrics
```

如果沒有，在 Deployment 加上 annotation，讓 Prometheus 知道要 scrape：

```yaml
spec:
  template:
    metadata:
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8000"
        prometheus.io/path: "/metrics"
```

---

### 5.2 更新 Gateway Collector，啟用 TargetAllocator

把 Module 2 建立的 `otel-gateway` 改成 2 個副本，並開啟 TargetAllocator：

```bash
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: otel-gateway
  namespace: observability
spec:
  mode: deployment
  replicas: 2  # 改成 2 個副本

  targetAllocator:
    enabled: true
    serviceAccount: otel-targetallocator
    prometheusCR:
      enabled: true          # 支援 ServiceMonitor/PodMonitor
      scrapeInterval: 30s
    allocationStrategy: consistent-hashing  # target 分配策略

  config:
    receivers:
      # prometheus receiver 改用 target allocator 模式
      prometheus:
        config:
          scrape_configs:
            - job_name: otel-managed
              # 向 TargetAllocator 取得自己應該 scrape 的 targets
        target_allocator:
          endpoint: http://otel-gateway-targetallocator.observability
          interval: 30s
          collector_id: ${env:POD_NAME}   # 每個副本用 pod name 作為 ID

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
      debug:
        verbosity: basic

    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [memory_limiter, k8sattributes, batch]
          exporters: [otlp/tempo]
        metrics:
          receivers: [otlp, prometheus]
          processors: [memory_limiter, k8sattributes, batch]
          exporters: [prometheusremotewrite, debug]
EOF
```

---

### 5.3 設定 TargetAllocator 的 RBAC

TargetAllocator 需要讀取 k8s 資源來發現 targets：

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: otel-targetallocator
  namespace: observability
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: otel-targetallocator
rules:
  - apiGroups: [""]
    resources:
      - pods
      - nodes
      - services
      - endpoints
      - namespaces
    verbs: ["get", "list", "watch"]
  - apiGroups: ["monitoring.coreos.com"]
    resources:
      - servicemonitors
      - podmonitors
    verbs: ["get", "list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: [endpointslices]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: otel-targetallocator
subjects:
  - kind: ServiceAccount
    name: otel-targetallocator
    namespace: observability
roleRef:
  kind: ClusterRole
  name: otel-targetallocator
  apiGroup: rbac.authorization.k8s.io
EOF
```

---

### 5.4 建立 PodMonitor，讓 TargetAllocator 發現 Demo 服務

```bash
kubectl apply -f - <<EOF
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: demo-services
  namespace: demo
spec:
  selector:
    matchLabels:
      app: order-service    # 只 scrape 有這個 label 的 pod
  podMetricsEndpoints:
    - port: http
      path: /metrics
      interval: 30s
---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: demo-inventory
  namespace: demo
spec:
  selector:
    matchLabels:
      app: inventory-service
  podMetricsEndpoints:
    - port: http
      path: /actuator/prometheus  # Spring Boot actuator 的 metrics endpoint
      interval: 30s
EOF
```

> 注意：需要先安裝 Prometheus Operator CRD（ServiceMonitor/PodMonitor 是它的 CRD）。
> 如果只安裝了 prometheus chart 而沒有 operator，需要改用靜態 scrape config。

---

### 5.5 觀察 TargetAllocator 的分配結果

TargetAllocator 有 HTTP API 可以查詢分配狀態：

```bash
kubectl -n observability port-forward svc/otel-gateway-targetallocator 8080:80 &

# 查看所有 targets
curl http://localhost:8080/jobs | jq .

# 查看特定 job 的 target 分配
curl http://localhost:8080/jobs/otel-managed/targets | jq .

# 應看到類似：
# {
#   "otel-gateway-collector-0": [
#     {"targets": ["order-service-pod-xxx:8000"], "labels": {...}}
#   ],
#   "otel-gateway-collector-1": [
#     {"targets": ["inventory-service-pod-yyy:8080"], "labels": {...}}
#   ]
# }
```

---

### 5.6 驗證沒有重複 scrape

```bash
# 在 Prometheus 查詢 target scrape 狀態
kubectl -n observability port-forward svc/prometheus-server 9090:80 &
# 開啟 http://localhost:9090/targets
# 確認每個 target 只出現一次
```

---

## Target 分配策略說明

| 策略 | 說明 | 適用場景 |
|------|------|---------|
| `least-weighted` | 分配給目前 target 數最少的 collector | 預設，適合 target 數量差異小的情況 |
| `consistent-hashing` | 用一致性 hash 分配，副本變更時遷移最少 | 副本數會動態變化時 |
| `per-node` | 每個 target 分配給同 node 的 collector | 搭配 DaemonSet 使用 |

---

## 進階練習

### 練習 A：模擬 Collector 副本掛掉

```bash
# 刪除一個 collector pod
kubectl -n observability delete pod otel-gateway-collector-0

# 觀察 TargetAllocator 如何重新分配
curl http://localhost:8080/jobs/otel-managed/targets | jq .
# 原本分配給 replica-0 的 targets 應該被移給 replica-1
```

### 練習 B：Scale Out

```bash
kubectl patch otelcol otel-gateway -n observability \
  --type=merge \
  -p='{"spec":{"replicas":3}}'

# 觀察 targets 如何重新分配到 3 個副本
```

---

## 完成檢查點

- [ ] `otel-gateway` 有 2 個副本
- [ ] TargetAllocator pod 正常運行
- [ ] TargetAllocator API 能看到 target 分配結果
- [ ] Prometheus 裡每個 demo 服務的 metric 只出現一份

---

## 下一步

→ [Module 6：OpAMP Bridge](./module-6-opamp.md) — 遠端動態管理 collector config
