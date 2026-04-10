# 第 2 章：CRD 資料模型（API Types）

> **本章目標：** 理解四個 CR 的 Spec/Status 結構，以及 kubebuilder marker 的作用

---

## 2.1 每個 CR 的共同模式

所有 CR 都長這樣：

```go
type XxxCR struct {
    metav1.TypeMeta   // apiVersion, kind
    metav1.ObjectMeta // name, namespace, labels, annotations, uid...
    Spec   XxxSpec    // 你希望的狀態（你填的）
    Status XxxStatus  // 目前實際的狀態（Operator 填的）
}
```

**重要概念：**
- **Spec 是輸入**：你告訴 Operator 想要什麼
- **Status 是輸出**：Operator 告訴你目前是什麼狀態
- 你只需要寫 Spec，永遠不要手動修改 Status

---

## 2.2 OpenTelemetryCollector（核心 CR）

**檔案：** [`apis/v1beta1/opentelemetrycollector_types.go`](../apis/v1beta1/opentelemetrycollector_types.go)

### Spec 結構拆解

```
OpenTelemetryCollectorSpec
│
├── [inline] OpenTelemetryCommonFields    ← apis/v1beta1/common.go:93
│   所有 OTel workload 共用的欄位
│   ├── ManagementState  "managed" | "unmanaged"
│   ├── Replicas         副本數（預設 1）
│   ├── Image            container image
│   ├── Resources        CPU / Memory limits
│   ├── Env              環境變數
│   ├── EnvFrom          從 ConfigMap/Secret 注入 env
│   ├── Volumes          額外 Volume
│   ├── VolumeMounts     Volume 掛載點
│   ├── Affinity         Pod 排程親和性
│   ├── Tolerations      Taint 容忍
│   ├── NodeSelector     節點選擇
│   ├── SecurityContext  Container 安全設定
│   ├── PodSecurityContext  Pod 安全設定
│   ├── InitContainers   初始化容器
│   └── AdditionalContainers  額外 sidecar 容器
│
├── [inline] StatefulSetCommonFields      ← apis/v1beta1/common.go:249
│   StatefulSet 專用欄位
│   ├── VolumeClaimTemplates   PVC 模板
│   └── PodManagementPolicy   Parallel | OrderedReady
│
├── Mode           "deployment" | "daemonset" | "statefulset" | "sidecar"
├── Config         OTel Collector YAML 設定（必填）
├── ConfigVersions 保留幾個版本的 ConfigMap（預設 3）
├── TargetAllocator 嵌入的 TA 設定
├── Autoscaler     HPA（Horizontal Pod Autoscaler）設定
├── Ingress        對外曝露設定
├── LivenessProbe  存活探針
├── ReadinessProbe 就緒探針
└── StartupProbe   啟動探針
```

### Mode 的影響

```go
// apis/v1beta1/mode.go:6-24
type Mode string

const (
    ModeDaemonSet   Mode = "daemonset"    // 每個 Node 一個 Pod
    ModeDeployment  Mode = "deployment"   // 固定副本數
    ModeSidecar     Mode = "sidecar"      // 注入到其他 Pod 內
    ModeStatefulSet Mode = "statefulset"  // 有穩定網路 ID 的 Pod
)
```

Mode 決定了 Operator 會建立哪種 k8s workload 資源：

| Mode | 建立的資源 | 適合場景 |
|---|---|---|
| `deployment` | Deployment | 無狀態 Collector，可水平擴展 |
| `daemonset` | DaemonSet | 每個 Node 都需要的 agent 模式 |
| `statefulset` | StatefulSet | 需要持久化儲存（如 Jaeger）|
| `sidecar` | 不建立資源 | 由 Webhook 注入到 Pod 內 |

### Status 結構

```go
// apis/v1beta1/opentelemetrycollector_types.go:50
type OpenTelemetryCollectorStatus struct {
    Scale   ScaleSubresourceStatus  // replicas 資訊
    Version string                  // 目前運行的 OTel 版本
    Image   string                  // 目前使用的 image
}
```

---

## 2.3 Instrumentation CR

**檔案：** [`apis/v1alpha1/instrumentation_types.go`](../apis/v1alpha1/instrumentation_types.go)

這個 CR **本身不建立任何 Pod**，只是一個設定模板。Webhook 在 Pod 建立時讀取它，把設定注入進去。

```
InstrumentationSpec
│
├── Exporter                       ← 遙測資料要送到哪
│   ├── Endpoint  "http://my-collector:4317"
│   └── TLS       TLS 憑證設定
│
├── Propagators                    ← 跨服務 context 傳遞方式
│   例如 [tracecontext, baggage, b3]
│
├── Sampler                        ← 取樣策略
│   ├── Type      "parentbased_traceidratio"
│   └── Argument  "0.25"（取樣 25%）
│
├── Resource                       ← OTel Resource Attributes
│   └── Attributes  {environment: "prod"}
│
├── Env                            ← 所有語言共用的 env vars（優先級最低）
│
└── 各語言設定（以 Java 為例）
    Java
    ├── Image    容器 image（內含 javaagent.jar）
    ├── Env      Java 專屬 env vars
    ├── Resources CPU/Memory
    └── Extensions  Java agent 擴展
```

### 環境變數的 4 層優先級

```
高優先級
  │  原始容器的 env（使用者自己設的）
  │  語言專屬 env（Java.Env、Python.Env...）
  │  共用 env（Instrumentation.Spec.Env）
  ↓  Instrumentation spec 推導的值（OTEL_SERVICE_NAME 等）
低優先級
```

前者定義了就不會被後者覆蓋。

---

## 2.4 TargetAllocator CR

**檔案：** [`apis/v1alpha1/targetallocator_types.go`](../apis/v1alpha1/targetallocator_types.go)

**使用場景：** 當你有多個 Collector 副本，需要分工抓取 Prometheus targets，避免重複或遺漏。

```
TargetAllocatorSpec
│
├── [inline] OpenTelemetryCommonFields  ← 跟 Collector 共用
│
├── AllocationStrategy              ← 如何分配 targets
│   ├── "consistent-hashing"        一致性雜湊（預設，支援 HA）
│   ├── "least-weighted"            分給負載最少的 Collector
│   └── "per-node"                  每個 Node 一個 Collector 抓
│
├── FilterStrategy  "relabel-config" ← 用 Prometheus relabel 規則過濾
│
├── ScrapeConfigs   []              ← 靜態 scrape 設定
├── PrometheusCR                    ← 是否監聽 ServiceMonitor/PodMonitor
└── CollectorNotReadyGracePeriod    ← Collector 掛掉後等多久才重新分配（預設 30s）
```

---

## 2.5 OpAMPBridge CR

**檔案：** [`apis/v1alpha1/opampbridge_types.go`](../apis/v1alpha1/opampbridge_types.go)

讓 OpAMP 管理平台可以遠端動態修改 Collector 設定，不需要更新 k8s CR。

```
OpAMPBridgeSpec
├── Endpoint      "ws://opamp-server:4320"  ← OpAMP server WebSocket 位址
├── Headers       認證 headers
├── Capabilities  這個 bridge 支援哪些 OpAMP 能力
│   例如 { AcceptsRemoteConfig: true, ReportsStatus: true }
└── ComponentsAllowed  允許哪些 OTel components 被遠端設定
    例如 { receivers: [otlp], exporters: [otlp] }
```

---

## 2.6 kubebuilder Markers

這些看起來像是註解的東西，其實是給 `controller-gen` 工具讀的，用來自動生成 CRD YAML 和 RBAC 規則。

```go
// apis/v1beta1/opentelemetrycollector_types.go:12-26

// +kubebuilder:object:root=true         ← 這是一個 CR 根物件
// +kubebuilder:resource:shortName=otelcol   ← 可用 kubectl get otelcol
// +kubebuilder:storageversion           ← 這是儲存用的版本
// +kubebuilder:subresource:status       ← Status 是獨立的子資源
// +kubebuilder:printcolumn:name="Mode",...  ← kubectl get 時顯示的欄位
```

```go
// 欄位驗證 markers
// +kubebuilder:validation:Minimum=1
Replicas *int32

// +kubebuilder:default:=3
ConfigVersions int  // 預設值 3

// +optional
Image string  // 可選欄位
```

```go
// 跨欄位驗證（CEL 表達式）
// apis/v1beta1/opentelemetrycollector_types.go:64-67
// +kubebuilder:validation:XValidation:rule="!(self.mode == 'sidecar' && self.affinity != null)"
// 意思是：sidecar 模式不能設定 affinity
```

**執行 `make update` 時，這些 marker 會被讀取，自動生成：**
- `config/crd/bases/*.yaml` — 實際安裝到 k8s 的 CRD
- RBAC 規則

---

                                                                                                                                                                                                                  
kubectl patch kafkanodepool dual-role-pool -n cattle-logging-system \                                                                                                                                          --type=merge                                                                                                                    -p '{"spec":{"storage":{"volumes":[{"id":0,"type":"persistent-claim","size":"8Gi","deleteClaim":false,"class":"nfs-csi"}]}}}'                                                                                   
    

## 2.7 四個 CR 的關係

```
OpenTelemetryCollector ─────────── 建立 ──────────→  Deployment / DaemonSet
       │                                              Service
       └── 嵌入 ──→  TargetAllocator（embedded）      ConfigMap
                     或獨立 CR                        ...

Instrumentation ──── 被 Webhook 讀取 ──→  注入到 Pod（不建立資源）

OpAMPBridge ─────────────────────────→  獨立 Deployment（連接 OpAMP server）
```

---

## 練習 1：對照程式碼理解 struct

打開 [`apis/v1beta1/common.go`](../apis/v1beta1/common.go)，找到 `OpenTelemetryCommonFields`（第 93 行）。

**問題：**

1. `AdditionalContainers` 和 `InitContainers` 有什麼差別？分別適合什麼使用場景？

2. 為什麼 `Replicas` 是 `*int32`（pointer），而不是 `int32`？
   提示：思考「沒有設定」和「設定為 0」有什麼差別。

<details>
<summary>參考答案</summary>

1. `InitContainers` 在主容器啟動**前**執行完畢（常用於初始化、等待依賴）；`AdditionalContainers` 與主容器同時運行（sidecar 模式）。

2. `*int32` 可以是 `nil`，代表「使用者沒有設定這個欄位」；`int32` 的 zero value 是 `0`，無法區分「使用者設定了 0」和「使用者沒有設定」。當值是 `nil` 時，Operator 可以套用預設值（預設 1 個副本）。
</details>

---

## 練習 2：寫一個 CR YAML

根據你對 `OpenTelemetryCollectorSpec` 的理解，寫一個 CR YAML，滿足以下需求：

1. 以 `daemonset` 模式部署
2. 每個 Pod 限制 CPU 200m、Memory 128Mi
3. OTel Collector config：接收 OTLP（gRPC 4317），輸出到 `debug` exporter
4. 加一個環境變數 `MY_ENV=hello`

```yaml
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: my-daemonset-collector
spec:
  # 填寫你的答案
```

<details>
<summary>參考答案</summary>

```yaml
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: my-daemonset-collector
spec:
  mode: daemonset
  resources:
    limits:
      cpu: 200m
      memory: 128Mi
  env:
    - name: MY_ENV
      value: hello
  config:
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: 0.0.0.0:4317
    exporters:
      debug: {}
    service:
      pipelines:
        traces:
          receivers: [otlp]
          exporters: [debug]
```
</details>

---

## 練習 3：理解 Instrumentation annotation 設定

根據 [`apis/v1alpha1/instrumentation_types.go`](../apis/v1alpha1/instrumentation_types.go)，寫一個 Instrumentation CR，滿足：

1. 遙測資料送到 `http://my-collector.monitoring:4317`
2. 啟用 `tracecontext` 和 `baggage` propagator
3. 取樣率 50%（使用 `parentbased_traceidratio`）
4. 設定 resource attribute `environment=production`

```yaml
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: my-instrumentation
spec:
  # 填寫你的答案
```

<details>
<summary>參考答案</summary>

```yaml
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: my-instrumentation
spec:
  exporter:
    endpoint: http://my-collector.monitoring:4317
  propagators:
    - tracecontext
    - baggage
  sampler:
    type: parentbased_traceidratio
    argument: "0.5"
  resource:
    resourceAttributes:
      environment: production
```
</details>

---

[← 第 1 章：Operator Pattern](./01-operator-pattern.md) | [第 3 章：Manifest 生成 →](./03-manifests-builder.md)
