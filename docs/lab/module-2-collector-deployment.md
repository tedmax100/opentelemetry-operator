# Module 2：Collector — Deployment 模式（集中式 Gateway）

## 目標

部署一個以 Deployment 模式運行的 OTel Collector，作為所有應用程式 telemetry 的集中 gateway。應用程式只需要知道 collector 的地址，由 collector 負責轉發到後端。

---

## 架構

```
order-service ──┐
                ├──► OTel Collector (Deployment) ──► Grafana Tempo
inventory-service ──┘         │
                               └──► Prometheus
```

Deployment 模式的特點：
- 以獨立 Pod 形式運行，可以 scale 副本數
- 適合作為集中 gateway 或 aggregator
- 所有節點的 telemetry 都匯集到這裡

---

## 步驟

### 2.1 建立 OpenTelemetryCollector CR（Deployment 模式）

```bash
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: otel-gateway
  namespace: observability
spec:
  mode: deployment
  replicas: 1

  config:
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: 0.0.0.0:4317
          http:
            endpoint: 0.0.0.0:4318

    processors:
      # 記憶體保護，避免 collector OOM
      memory_limiter:
        check_interval: 1s
        limit_mib: 400
        spike_limit_mib: 100

      # 批次處理，提升傳輸效率
      batch:
        timeout: 5s
        send_batch_size: 512

      # 從 k8s API 補充 pod/node metadata
      k8sattributes:
        auth_type: serviceAccount
        passthrough: false
        extract:
          metadata:
            - k8s.pod.name
            - k8s.pod.uid
            - k8s.namespace.name
            - k8s.node.name
            - k8s.deployment.name
          labels:
            - tag_name: app.kubernetes.io/name
              key: app.kubernetes.io/name
              from: pod
            - tag_name: app.kubernetes.io/version
              key: app.kubernetes.io/version
              from: pod

      # 加入 collector 自身資訊
      resource:
        attributes:
          - key: collector.mode
            value: gateway
            action: insert

    exporters:
      # Traces → Tempo
      otlp/tempo:
        endpoint: tempo.observability:4317
        tls:
          insecure: true

      # Metrics → Prometheus（以 remote_write 方式）
      prometheusremotewrite:
        endpoint: http://prometheus-server.observability/api/v1/write

      # Debug 用，在 collector log 印出（學習環境才開）
      debug:
        verbosity: normal

    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [memory_limiter, k8sattributes, resource, batch]
          exporters: [otlp/tempo, debug]
        metrics:
          receivers: [otlp]
          processors: [memory_limiter, k8sattributes, resource, batch]
          exporters: [prometheusremotewrite, debug]
EOF
```

---

### 2.2 確認 Collector 部署成功

```bash
kubectl -n observability get pod -l app.kubernetes.io/name=otel-gateway-collector
# 應看到 1 個 Running pod

kubectl -n observability logs -l app.kubernetes.io/name=otel-gateway-collector --tail=20
# 應看到 "Everything is ready. Begin running and processing data."
```

Operator 自動建立的 Service：

```bash
kubectl -n observability get svc | grep otel-gateway
# otel-gateway-collector         ClusterIP   ...   4317/TCP,4318/TCP
# otel-gateway-collector-headless ClusterIP  ...   4317/TCP,4318/TCP
# otel-gateway-collector-monitoring ClusterIP ... 8888/TCP  ← collector 自身 metrics
```

---

### 2.3 更新 Instrumentation CR，改指向 Collector

```bash
kubectl patch instrumentation demo-instrumentation -n demo \
  --type=merge \
  -p='{"spec":{"exporter":{"endpoint":"http://otel-gateway-collector.observability:4317"}}}'
```

重啟 demo 服務讓新的 endpoint 生效：

```bash
kubectl rollout restart deployment/order-service -n demo
kubectl rollout restart deployment/inventory-service -n demo
```

---

### 2.4 驗證 Telemetry 流向

產生流量：

```bash
kubectl -n demo port-forward svc/order-service 8000:8000 &
curl -X POST http://localhost:8000/orders \
  -H "Content-Type: application/json" \
  -d '{"product_id": "P001", "quantity": 3}'
```

查看 collector log，確認有收到資料：

```bash
kubectl -n observability logs -l app.kubernetes.io/name=otel-gateway-collector -f
# 應看到 debug exporter 印出 trace/metric 資料
```

在 Grafana Tempo 確認 trace 仍然可見，且多了 k8s metadata（pod name、namespace 等）。

---

### 2.5 觀察 k8sattributes 的效果

在 Tempo 搜尋一個 trace，點開 span attributes，應看到：

```
k8s.pod.name: order-service-7d9f8b-xxxxx
k8s.namespace.name: demo
k8s.deployment.name: order-service
k8s.node.name: k3d-otel-lab-agent-0
collector.mode: gateway
```

這些 attribute 是由 `k8sattributes` processor 自動補充的，應用程式本身不需要知道這些資訊。

---

### 2.6 Collector 自身 Metrics

Collector 會在 `:8888/metrics` 暴露自身的 Prometheus metrics：

```bash
kubectl -n observability port-forward svc/otel-gateway-collector-monitoring 8888:8888 &
curl http://localhost:8888/metrics | grep otelcol_receiver_accepted
# otelcol_receiver_accepted_spans_total{...} 42
```

在後續 Module 3 加入 Node Agent 後，這些 metrics 也會被 scrape 進 Prometheus。

---

## 認識 Processor 的順序

Pipeline 裡 processor 的順序很重要：

```yaml
processors: [memory_limiter, k8sattributes, resource, batch]
```

| 順序 | Processor | 原因 |
|------|-----------|------|
| 1st | memory_limiter | 要最先跑，確保 OOM 時能丟棄資料 |
| 2nd | k8sattributes | 補充 metadata，讓後續 processor 可以用 |
| 3rd | resource | 加入自訂 attribute |
| Last | batch | 最後才批次，確保 metadata 都補充好了 |

---

## 進階練習

### 練習 A：加入 Filter Processor

只保留 demo namespace 的 trace：

```yaml
processors:
  filter/demo-only:
    error_mode: ignore
    traces:
      span:
        - 'attributes["k8s.namespace.name"] != "demo"'
```

加到 traces pipeline 的 processors 裡。

### 練習 B：Collector Scale Out

把 replica 改成 2，觀察 headless service 和 ClusterIP service 的差異：

```bash
kubectl patch otelcol otel-gateway -n observability \
  --type=merge \
  -p='{"spec":{"replicas":2}}'
```

> 思考：兩個 collector replica 都會收到 trace，這樣 tail-based sampling 會有什麼問題？（TargetAllocator 在 Module 5 解決類似的問題。）

### 練習 C：加入 Tail-Based Sampling

```yaml
processors:
  tail_sampling:
    decision_wait: 10s
    policies:
      - name: errors-policy
        type: status_code
        status_code: {status_codes: [ERROR]}
      - name: slow-traces-policy
        type: latency
        latency: {threshold_ms: 500}
```

只保留有 error 或 latency > 500ms 的 trace。

---

## 完成檢查點

- [ ] `otel-gateway` collector pod 正常運行
- [ ] Instrumentation CR 指向 collector endpoint
- [ ] Grafana Tempo 仍可看到 trace
- [ ] Trace 的 span attributes 包含 k8s metadata
- [ ] Collector log 有顯示收到 spans

---

## 下一步

現在我們有了集中 gateway，但 host-level metrics（CPU、memory、disk）還沒有。這需要在每個 node 上跑一個 agent。

→ [Module 3：Collector — DaemonSet 模式](./module-3-collector-daemonset.md)
