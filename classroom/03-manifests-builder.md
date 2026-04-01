# 第 3 章：Manifest 生成（Spec → k8s 資源）

> **本章目標：** 理解 CR Spec 如何被轉換成實際的 Kubernetes 資源（Deployment、Service、ConfigMap...）

---

## 3.1 整體架構

Manifest 生成層的職責：**給定 CR Spec，計算出應該有哪些 k8s 資源**。

這層的所有函式都是**純函式（Pure Function）**：
- 只接收 `params`（包含 CR spec），只回傳 k8s 物件
- **不呼叫任何 k8s API**
- 無副作用，可以安全地在 unit test 中測試

```
internal/manifests/collector/
├── collector.go    ← Build() 入口，根據 Mode 決定生成哪些資源
├── deployment.go   ← 生成 Deployment
├── container.go    ← 生成 Container spec
├── configmap.go    ← 生成 ConfigMap（含 SHA 命名）
├── service.go      ← 生成 Service
├── daemonset.go    ← 生成 DaemonSet
├── statefulset.go  ← 生成 StatefulSet
└── ...其他資源
```

---

## 3.2 工廠模式（Factory Pattern）

**檔案：** [`internal/manifests/builder.go`](../internal/manifests/builder.go)

這個專案用了一個簡潔的工廠模式來統一管理資源生成：

```go
// 類型定義
type ManifestFactory[T client.Object, Params any] func(params Params) (T, error)
type K8sManifestFactory[Params any] ManifestFactory[client.Object, Params]

// Factory() 是包裝函式，把具體類型的 factory 轉成通用的 K8sManifestFactory
func Factory[T client.Object, Params any](f ManifestFactory[T, Params]) K8sManifestFactory[Params] {
    return func(params Params) (client.Object, error) {
        return f(params)
    }
}
```

**為什麼這樣設計？**  
每個資源函式（`Deployment`、`Service`...）回傳的是具體類型（`*appsv1.Deployment`），但我們需要用一個 `[]client.Object` 統一處理。`Factory()` 負責做這個轉換，讓我們可以把所有工廠函式放進同一個 slice。

---

## 3.3 Build() — 決策入口

**檔案：** [`internal/manifests/collector/collector.go`](../internal/manifests/collector/collector.go)

```go
func Build(params manifests.Params) ([]client.Object, error) {
    var manifestFactories []manifests.K8sManifestFactory[manifests.Params]

    // 根據 Mode 決定加哪種 workload
    switch params.OtelCol.Spec.Mode {
    case v1beta1.ModeDeployment:
        manifestFactories = append(manifestFactories,
            manifests.Factory(Deployment),
            manifests.Factory(PodDisruptionBudget),
        )
    case v1beta1.ModeStatefulSet:
        manifestFactories = append(manifestFactories,
            manifests.Factory(StatefulSet),
            manifests.Factory(PodDisruptionBudget),
        )
    case v1beta1.ModeDaemonSet:
        manifestFactories = append(manifestFactories,
            manifests.Factory(DaemonSet),
        )
    case v1beta1.ModeSidecar:
        // sidecar 不在這裡處理，由 Webhook 負責
    }

    // 不管哪種 Mode，這些資源一定會建立
    manifestFactories = append(manifestFactories,
        manifests.Factory(ConfigMap),
        manifests.Factory(HorizontalPodAutoscaler),
        manifests.Factory(ServiceAccount),
        manifests.Factory(Service),
        manifests.Factory(HeadlessService),
        // ...
    )

    // 條件性資源
    if params.OtelCol.Spec.Observability.Metrics.EnableMetrics {
        manifestFactories = append(manifestFactories,
            manifests.Factory(ServiceMonitor),
        )
    }

    // 逐一呼叫所有 factory，收集結果
    for _, factory := range manifestFactories {
        res, err := factory(params)
        if manifests.ObjectIsNotNil(res) {
            resourceManifests = append(resourceManifests, res)
        }
    }
    return resourceManifests, nil
}
```

**注意 `ObjectIsNotNil(res)` 的檢查：** 某些 factory 在條件不符合時會回傳 `nil`（例如 HPA 在沒有設定 autoscaler 時），這個檢查避免把 nil 加進結果。

---

## 3.4 Deployment() — CR Spec 到 Deployment

**檔案：** [`internal/manifests/collector/deployment.go`](../internal/manifests/collector/deployment.go)

```go
func Deployment(params manifests.Params) (*appsv1.Deployment, error) {
    name := naming.Collector(params.OtelCol.Name)  // 加前綴命名

    return &appsv1.Deployment{
        ObjectMeta: metav1.ObjectMeta{
            Name:      name,
            Namespace: params.OtelCol.Namespace,
            Labels:    labels,       // 含版本、app 等標準 labels
            Annotations: annotations,
        },
        Spec: appsv1.DeploymentSpec{
            Replicas: manifestutils.GetDesiredReplicas(params.OtelCol),
            Selector: &metav1.LabelSelector{
                MatchLabels: manifestutils.SelectorLabels(...),
            },
            Strategy: params.OtelCol.Spec.DeploymentUpdateStrategy,
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    // 直接從 CR Spec 映射
                    Tolerations:               params.OtelCol.Spec.Tolerations,
                    NodeSelector:              params.OtelCol.Spec.NodeSelector,
                    Affinity:                  params.OtelCol.Spec.Affinity,
                    SecurityContext:           params.OtelCol.Spec.PodSecurityContext,
                    PriorityClassName:         params.OtelCol.Spec.PriorityClassName,
                    TerminationGracePeriodSeconds: params.OtelCol.Spec.TerminationGracePeriodSeconds,
                    InitContainers:            params.OtelCol.Spec.InitContainers,
                    // 主容器 + 使用者額外的 sidecar
                    Containers: append(
                        []corev1.Container{Container(params.Config, params.Log, params.OtelCol, true)},
                        params.OtelCol.Spec.AdditionalContainers...,
                    ),
                    Volumes: Volumes(params.Config, params.OtelCol),
                },
            },
        },
    }, nil
}
```

大部分欄位都是直接把 `CR Spec` 的值複製過去。真正有邏輯的是 `Container()` 和 `Volumes()`。

---

## 3.5 Container() — 主容器的生成

**檔案：** [`internal/manifests/collector/container.go`](../internal/manifests/collector/container.go)

### Image 決定邏輯

```go
image := otelcol.Spec.Image
if image == "" {
    image = cfg.CollectorImage  // 使用 operator 預設 image
}
```

### 啟動參數（Args）

```go
// config 參數永遠是第一個，指向 ConfigMap 的掛載路徑
args = append(args, fmt.Sprintf("--config=/conf/%s", cfg.CollectorConfigMapEntry))

// 使用者自訂的 args（排序後加入，確保順序確定，避免不必要的 reconcile）
var sortedArgs []string
for k, v := range argsMap {
    sortedArgs = append(sortedArgs, fmt.Sprintf("--%s=%s", k, v))
}
slices.Sort(sortedArgs)
args = append(args, sortedArgs...)
```

**為什麼要排序？** Map 的迭代順序在 Go 中是不確定的。如果每次 Reconcile 產生的 args 順序不同，controller 會誤以為有變化，觸發不必要的 Update。

### Env Var 合併邏輯

```go
// 使用者定義的 env（優先）
envVars = append(envVars, otelcol.Spec.Env...)

// 自動推導的 env（只加入使用者沒有定義的）
for _, env := range inferredEnvVars {
    if _, ok := userDefinedEnvVars[env.Name]; !ok {
        envVars = append(envVars, env)
    }
}
```

自動推導的 env 包含（`container.go:325-366`）：
- `POD_NAME` — 來自 `metadata.name` fieldRef
- `GOMEMLIMIT` / `GOMAXPROCS` — 若啟用 feature gate，自動根據 resource limits 設定
- 從 OTel Config YAML 解析出的變數
- HTTP Proxy 設定（`HTTP_PROXY`、`HTTPS_PROXY`）

### Port 決定邏輯

```go
// 1. 從 OTel Config YAML 自動解析（例如 otlp receiver 開了 4317）
ports, _ := getConfigContainerPorts(logger, otelcol.Spec.Config)

// 2. 使用者在 Spec.Ports 手動指定的
if len(otelcol.Spec.Ports) > 0 {
    // 合併，Port 號衝突→ 以使用者為準（丟棄自動解析的）
    // Port 名衝突→ 自動解析的改名為 "port-{號碼}"
    ports = merge(otelcol.Spec.Ports, ports)
}
```

---

## 3.6 ConfigMap() — 特殊的 SHA 命名

**檔案：** [`internal/manifests/collector/configmap.go`](../internal/manifests/collector/configmap.go)

```go
func ConfigMap(params manifests.Params) (*corev1.ConfigMap, error) {
    // 計算 config 內容的 SHA hash
    hash, _ := manifestutils.GetConfigMapSHA(otelCol.Spec.Config)

    // ConfigMap 名稱 = collector名稱 + hash 前幾個字元
    // 例如：my-collector-7d9f2a1b
    name := naming.ConfigMap(otelCol.Name, hash)

    // 把 OTel Collector YAML 存進 ConfigMap
    return &corev1.ConfigMap{
        Data: map[string]string{
            "collector.yaml": replacedConf,
        },
    }, nil
}
```

**為什麼 ConfigMap 名稱包含 SHA？**

每次 config 內容改變，SHA 就不同，會建立一個**新的** ConfigMap，而不是更新舊的。

好處：
1. 可以保留舊版本的 ConfigMap（`ConfigVersions` 欄位控制保留幾個）
2. 萬一新 config 有問題，可以回滾
3. 讓 Operator 追蹤「哪個 Pod 用的是哪個版本的 config」

---

## 3.7 TargetAllocator Config 替換

**檔案：** [`internal/manifests/collector/config_replace.go`](../internal/manifests/collector/config_replace.go)

當 TargetAllocator 啟用時，Collector 的 Prometheus receiver 設定需要指向 TA 的 endpoint，但使用者通常不知道 TA 的位址。Operator 自動做這個替換：

```yaml
# 使用者寫的：
receivers:
  prometheus:
    config:
      scrape_configs:
        - job_name: 'myapp'
          static_configs: ...

# Operator 替換後（ConfigMap 裡的實際內容）：
receivers:
  prometheus:
    config:
      scrape_configs: []  # 清空，由 TA 動態分配
    target_allocator:
      endpoint: http://my-collector-targetallocator:80
      interval: 30s
```

---

## 練習 1：閱讀理解

打開 [`internal/manifests/collector/container.go`](../internal/manifests/collector/container.go)，找到 `getContainerPorts()` 函式（第 197 行）。

**問題：** 當使用者在 `Spec.Ports` 中指定了 port name `"otlp-grpc"`，而 Operator 從 config 自動解析也得到一個叫 `"otlp-grpc"` 的 port，最終結果是什麼？

找到 `filterContainerPort()` 函式（第 234 行），理解衝突處理邏輯後回答。

<details>
<summary>參考答案</summary>

當 port 名衝突時（`portNames[candidate.Name]` 為 true），Operator 會嘗試用 fallback 名稱 `port-{號碼}`（例如 `port-4317`）。如果 fallback 名稱也衝突，則直接丟棄自動解析的 port。

最終結果：使用者在 `Spec.Ports` 指定的 `otlp-grpc` 保留，Operator 自動解析的 `otlp-grpc` 被重命名為 `port-4317`（若此名未被佔用），或直接丟棄。
</details>

---

## 練習 2：撰寫單元測試

這層的函式都是純函式，非常適合單元測試。仿照 [`internal/manifests/collector/deployment_test.go`](../internal/manifests/collector/deployment_test.go) 的模式，嘗試寫一個測試：

**目標：** 驗證當 `Spec.Mode = "sidecar"` 時，`Build()` 不會包含 Deployment。

```go
// 在 internal/manifests/collector/ 目錄下新建 collector_exercise_test.go
package collector_test

import (
    "testing"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    appsv1 "k8s.io/api/apps/v1"
    // ... 其他 import
)

func TestBuildSidecarMode_NoDeployment(t *testing.T) {
    // 提示：
    // 1. 建立一個 Params，設定 OtelCol.Spec.Mode = v1beta1.ModeSidecar
    // 2. 呼叫 collector.Build(params)
    // 3. 確認結果中沒有 *appsv1.Deployment 類型的物件

    params := manifests.Params{
        OtelCol: v1beta1.OpenTelemetryCollector{
            Spec: v1beta1.OpenTelemetryCollectorSpec{
                Mode: v1beta1.ModeSidecar,
                // ...
            },
        },
        // ...
    }

    objects, err := collector.Build(params)
    require.NoError(t, err)

    for _, obj := range objects {
        _, isDeployment := obj.(*appsv1.Deployment)
        assert.False(t, isDeployment, "sidecar mode should not create a Deployment")
    }
}
```

<details>
<summary>提示：參考現有測試的 Params 建構方式</summary>

看 `internal/manifests/collector/suite_test.go` 中的 test helper，或是 `deployment_test.go` 中的 `params` 建構，了解如何建立最小可用的 `manifests.Params`。

需要的最小欄位：
- `OtelCol.Spec.Mode`
- `OtelCol.Spec.Config`（至少是空物件）
- `Config`（operator config）
- `Scheme`
</details>

---

## 練習 3：追蹤一個欄位的旅程

選擇 `Spec.Tolerations`，從 CR 追蹤到最終的 Deployment YAML：

1. 在 [`apis/v1beta1/common.go`](../apis/v1beta1/common.go) 找到 `Tolerations` 的定義（含 kubebuilder marker）
2. 在 [`internal/manifests/collector/deployment.go`](../internal/manifests/collector/deployment.go) 找到它被使用的地方
3. DaemonSet 也有 `Tolerations` 嗎？打開 [`internal/manifests/collector/daemonset.go`](../internal/manifests/collector/daemonset.go) 確認

---

[← 第 2 章：CRD 資料模型](./02-crd-api-types.md) | [第 4 章：Webhook 注入 →](./04-webhook-injection.md)
