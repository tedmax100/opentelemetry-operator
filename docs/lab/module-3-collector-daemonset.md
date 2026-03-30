# Module 3：Collector — DaemonSet 模式（Node Agent）

## 目標

在每個 Kubernetes node 部署一個 OTel Collector，收集 host-level metrics（CPU、memory、disk、network），以及 kubelet 統計數據，並統一 forward 到 Module 2 建立的 gateway collector。

---

## 架構

```
Node 1                          Node 2
┌─────────────────────┐         ┌─────────────────────┐
│  otel-agent (pod)   │         │  otel-agent (pod)   │
│  - hostmetrics      │         │  - hostmetrics      │
│  - kubeletstats     │         │  - kubeletstats     │
└─────────┬───────────┘         └─────────┬───────────┘
          │                               │
          └───────────┬───────────────────┘
                      ▼
           otel-gateway (Deployment)
                      │
           ┌──────────┴──────────┐
           ▼                     ▼
       Grafana Tempo          Prometheus
```

DaemonSet 模式的特點：
- 每個 node 恰好一個 pod（由 Kubernetes DaemonSet 保證）
- 可以 mount node 的 host filesystem
- 可以存取 kubelet API

---

## 步驟

### 3.1 建立 OpenTelemetryCollector CR（DaemonSet 模式）

```bash
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: otel-agent
  namespace: observability
spec:
  mode: daemonset

  # 掛載 host filesystem，讓 hostmetricsreceiver 能讀取系統資訊
  volumeMounts:
    - name: hostfs
      mountPath: /hostfs
      readOnly: true
      mountPropagation: HostToContainer

  volumes:
    - name: hostfs
      hostPath:
        path: /

  # 需要讀取 /proc /sys 等目錄
  env:
    - name: NODE_NAME
      valueFrom:
        fieldRef:
          fieldPath: spec.nodeName

  config:
    receivers:
      # Host 層級 metrics：CPU、memory、disk、network、filesystem
      hostmetrics:
        root_path: /hostfs
        collection_interval: 30s
        scrapers:
          cpu:
            metrics:
              system.cpu.utilization:
                enabled: true
          memory:
            metrics:
              system.memory.utilization:
                enabled: true
          disk: {}
          filesystem:
            exclude_mount_points:
              mount_points: [/dev/*, /proc/*, /sys/*, /run/k3s/containerd/*]
              match_type: regexp
          network: {}
          load: {}
          processes: {}

      # Kubelet 統計：pod/container 層級的 CPU、memory
      kubeletstats:
        auth_type: serviceAccount
        collection_interval: 30s
        endpoint: https://${env:NODE_NAME}:10250
        insecure_skip_verify: true
        metric_groups:
          - node
          - pod
          - container

    processors:
      memory_limiter:
        check_interval: 1s
        limit_mib: 200
        spike_limit_mib: 50

      batch:
        timeout: 10s
        send_batch_size: 256

      # 加入 node name，讓 metrics 可以按 node 過濾
      resource:
        attributes:
          - key: k8s.node.name
            value: ${env:NODE_NAME}
            action: upsert
          - key: collector.mode
            value: agent
            action: insert

    exporters:
      # Forward 到 gateway collector，而不是直接送後端
      otlp/gateway:
        endpoint: otel-gateway-collector.observability:4317
        tls:
          insecure: true

      debug:
        verbosity: basic

    service:
      pipelines:
        metrics:
          receivers: [hostmetrics, kubeletstats]
          processors: [memory_limiter, resource, batch]
          exporters: [otlp/gateway, debug]
EOF
```

---

### 3.2 設定 RBAC

kubeletstats receiver 需要存取 kubelet API，以及讀取 node/pod 資訊：

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: otel-agent
  namespace: observability
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: otel-agent
rules:
  - apiGroups: [""]
    resources:
      - nodes
      - nodes/stats
      - nodes/proxy
      - services
      - endpoints
      - pods
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: [replicasets]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: otel-agent
subjects:
  - kind: ServiceAccount
    name: otel-agent
    namespace: observability
roleRef:
  kind: ClusterRole
  name: otel-agent
  apiGroup: rbac.authorization.k8s.io
EOF
```

更新 DaemonSet 使用這個 ServiceAccount：

```bash
kubectl patch otelcol otel-agent -n observability \
  --type=merge \
  -p='{"spec":{"serviceAccount":"otel-agent"}}'
```

---

### 3.3 確認 DaemonSet 部署

```bash
kubectl -n observability get daemonset
# NAME                        DESIRED   CURRENT   READY
# otel-agent-collector        2         2         2     ← 2 個 node，各一個

kubectl -n observability get pod -l app.kubernetes.io/name=otel-agent-collector -o wide
# 應看到每個 node 都有一個 pod
```

---

### 3.4 確認 Metrics 流向 Gateway

查看 gateway collector 的 log，確認有收到來自 agent 的 metrics：

```bash
kubectl -n observability logs -l app.kubernetes.io/name=otel-gateway-collector --tail=30 | grep "system\."
```

或直接查 Prometheus：

```bash
kubectl -n observability port-forward svc/prometheus-server 9090:80 &
# 開啟 http://localhost:9090
# 搜尋 system_cpu_utilization 或 k8s_pod_cpu_utilization
```

---

### 3.5 在 Grafana 建立 Node Dashboard

進入 Grafana → Import Dashboard，輸入 ID `1860`（Node Exporter Full）或建立自訂 dashboard：

**常用 metrics 查詢範例：**

```promql
# 各 node 的 CPU 使用率
system_cpu_utilization{state="user"} * 100

# 各 pod 的 memory 使用量
k8s_pod_memory_working_set_bytes

# 各 container 的 CPU 限制使用率
rate(k8s_container_cpu_time_seconds_total[5m])
```

---

## 理解 Agent → Gateway 的分層架構

```
為什麼 agent 不直接送 Tempo/Prometheus？

應用 telemetry ──► Gateway ──► Tempo
                      ▲          └──► Prometheus
Node metrics ──────── │
                   在這裡統一：
                   - batching
                   - enrichment
                   - routing
                   - retry logic
```

好處：
- Agent 保持輕量，只負責收集
- 後端地址只需在 gateway 設定一次
- Gateway 可以加 filter、transform，agent 不需要知道
- Gateway 掛掉時，agent 會自動重試

---

## 進階練習

### 練習 A：加入 Container Log 收集

在 agent 加入 filelog receiver，收集 container log：

```yaml
receivers:
  filelog:
    include:
      - /var/log/pods/demo_*/*/*.log
    include_file_path: true
    operators:
      - type: container
        id: container-parser
```

需要額外掛載 `/var/log/pods`：

```yaml
volumeMounts:
  - name: varlogpods
    mountPath: /var/log/pods
    readOnly: true
volumes:
  - name: varlogpods
    hostPath:
      path: /var/log/pods
```

### 練習 B：觀察 DaemonSet 的排程行為

新增一個 node（k3d 支援動態加 node）：

```bash
k3d node create otel-lab-agent-2 --cluster otel-lab
```

觀察 DaemonSet 自動在新 node 建立 agent pod：

```bash
kubectl -n observability get pod -l app.kubernetes.io/name=otel-agent-collector -w
```

---

## 完成檢查點

- [ ] 每個 node 都有一個 `otel-agent-collector` pod
- [ ] Prometheus 可查到 `system_cpu_utilization` metrics
- [ ] Prometheus 可查到 `k8s_pod_memory_working_set_bytes` metrics
- [ ] Grafana 可建立 node metrics dashboard

---

## 下一步

我們現在有了 Deployment（gateway）和 DaemonSet（node agent）兩種模式。還有一種模式是 Sidecar，直接注入到應用程式 pod 裡。

→ [Module 4：Collector — Sidecar 模式](./module-4-collector-sidecar.md)

或跳過 Sidecar，直接學習 TargetAllocator：

→ [Module 5：TargetAllocator](./module-5-target-allocator.md)
