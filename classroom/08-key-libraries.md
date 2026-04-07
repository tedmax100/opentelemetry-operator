# 第 8 章：重要 Library 解析

> **本章目標：** 理解 OTel Operator 依賴的核心 library 各自的職責，以及它們在程式碼中的實際用法

這份 Operator 的依賴看似很多，但核心邏輯只依賴少數幾個 library。掌握這些，你就能讀懂 90% 的程式碼。

---

## 8.1 全景：誰負責什麼

```
┌─────────────────────────────────────────────────────────────┐
│                你寫的 Operator 邏輯                           │
└──────┬─────────────────────────────────────────────────────┘
       │ 建立在
       ▼
┌─────────────────────────────────────────────────────────────┐
│          controller-runtime  (框架層)                         │
│   Manager / Reconciler / Client / Predicate / Webhook ...   │
└──────┬──────────────────────────────────────────────────────┘
       │ 建立在
       ▼
┌─────────────────────────────────────────────────────────────┐
│          client-go  (K8s 客戶端層)                            │
│   Informer / Reflector / WorkQueue / DeltaFIFO / retry ...  │
└──────┬──────────────────────────────────────────────────────┘
       │ 使用
       ▼
┌─────────────────────────────────────────────────────────────┐
│  k8s.io/api          k8s.io/apimachinery                    │
│  （資源型別定義）      （通用工具：序列化、比較、錯誤處理）       │
└─────────────────────────────────────────────────────────────┘

工具層（各自獨立）：
  go-logr/logr          ← 結構化日誌介面
  dario.cat/mergo       ← 結構體深度合併
  Masterminds/semver    ← 語意化版本比較
  k8s.io/client-go/util/retry  ← 衝突重試
```

---

## 8.2 `sigs.k8s.io/controller-runtime`：Operator 框架

**你使用的別名：** `ctrl "sigs.k8s.io/controller-runtime"`

這是整個 Operator 的骨架。它把 `client-go` 的低層複雜性包裝成清晰的 API，讓你專注寫業務邏輯。

### Manager：所有元件的容器

```go
// main.go
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    Scheme:                 scheme,
    MetricsBindAddress:     metricsAddr,
    HealthProbeBindAddress: probeAddr,
    LeaderElection:         enableLeaderElection,
})
```

`Manager` 負責：
- 啟動並管理所有 Controller 的生命週期
- 維護所有 Controller 共用的 Informer 快取（節省資源，同一種資源只 Watch 一次）
- 處理 Leader Election（在多副本 Operator 中確保只有一個在執行）
- 提供健康檢查端點（`/healthz`、`/readyz`）

### client.Client：與 K8s API 互動的唯一入口

```go
// internal/controllers/opentelemetrycollector_controller.go

// 讀（從本地 Informer 快取讀，不打 API Server）
var instance v1beta1.OpenTelemetryCollector
err := r.Get(ctx, req.NamespacedName, &instance)

// 寫（直接打 API Server，繞過快取）
err := r.Update(ctx, &instance)
err := r.Create(ctx, &deployment)
err := r.Delete(ctx, &service)

// 列出（從快取讀，可加 label selector）
var deployments appsv1.DeploymentList
err := r.List(ctx, &deployments,
    client.InNamespace("default"),
    client.MatchingLabels{"app": "otel-collector"},
)
```

**關鍵設計：** `Get` 和 `List` 從 Informer 的本地快取讀取，速度極快，不會增加 API Server 負擔。`Create`、`Update`、`Delete` 才會真正打 API Server。

### ctrl.CreateOrUpdate：冪等的建立或更新

```go
// internal/controllers/common.go:144
result, err := ctrl.CreateOrUpdate(ctx, kubeClient, existing, mutateFn)
// result 會是 "created" / "updated" / "unchanged" 其中之一
```

`CreateOrUpdate` 的運作邏輯：
1. 嘗試 `Get(existing)`
2. 如果 `NotFound` → 呼叫 `Create`
3. 如果找到 → 執行 `mutateFn()`（你的更新邏輯）→ 呼叫 `Update`

這讓 `Reconcile` 可以安全地重複執行，不需要自己判斷「應該 Create 還是 Update」。

### ctrl.SetControllerReference：設定 OwnerReference

```go
// internal/controllers/common.go:132
ctrl.SetControllerReference(owner, desired, scheme)
```

呼叫後，`desired` 物件的 `metadata.ownerReferences` 會被設定為指向 `owner`（即 CR）。這讓 K8s GC 在 CR 被刪除時自動清理所有子資源。

---

## 8.3 `k8s.io/client-go`：K8s 客戶端底層

controller-runtime 底層使用 `client-go`，但大多時候你只會直接用到它的兩個子套件。

### `client-go/util/retry`：樂觀鎖衝突重試

```go
// internal/controllers/common.go:143
import "k8s.io/client-go/util/retry"

crudErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
    result, err := ctrl.CreateOrUpdate(ctx, kubeClient, existing, mutateFn)
    return err
})
```

**為什麼需要這個？**

K8s 使用樂觀鎖。每個物件都有 `resourceVersion`，Update 時必須帶上當前版本號。如果在你讀取之後、Update 之前，有其他 Controller 也修改了同一個物件，K8s 會回傳 `409 Conflict`。

`retry.RetryOnConflict` 在遇到 409 時，自動重新讀取最新版本再試一次。`retry.DefaultRetry` 預設最多重試 5 次，採用指數退避（backoff）策略。

```
第 1 次失敗 → 等 10ms → 第 2 次
第 2 次失敗 → 等 20ms → 第 3 次
第 3 次失敗 → 等 40ms → 第 4 次
...
```

### `client-go/tools/events`：K8s Events

```go
// pkg/collector/upgrade/upgrade.go
type VersionUpgrade struct {
    Recorder events.EventRecorder
    ...
}

// 使用
u.Recorder.Eventf(&instance, corev1.EventTypeNormal, "Upgraded",
    "Upgraded OpenTelemetryCollector to version %s", newVersion)
```

這讓你可以用 `kubectl describe` 看到 Operator 發出的事件：

```
Events:
  Type    Reason    Age   From                 Message
  ----    ------    ----  ----                 -------
  Normal  Upgraded  2m    otel-operator  Upgraded to version 0.96.0
```

---

## 8.4 `k8s.io/api`：K8s 資源型別定義

這個套件包含所有 K8s 內建資源的 Go struct，按 API group 分成子套件：

```go
import (
    appsv1   "k8s.io/api/apps/v1"     // Deployment, DaemonSet, StatefulSet
    corev1   "k8s.io/api/core/v1"     // Pod, Service, ConfigMap, Secret, ServiceAccount
    rbacv1   "k8s.io/api/rbac/v1"     // ClusterRole, ClusterRoleBinding
    networkingv1 "k8s.io/api/networking/v1" // Ingress, NetworkPolicy
    autoscalingv2 "k8s.io/api/autoscaling/v2" // HorizontalPodAutoscaler
    policyV1 "k8s.io/api/policy/v1"   // PodDisruptionBudget
)
```

**範例：建立一個 Deployment struct**

```go
// internal/manifests/collector/deployment.go（節錄）
deployment := &appsv1.Deployment{
    ObjectMeta: metav1.ObjectMeta{
        Name:      naming.Collector(params.OtelCol.Name),
        Namespace: params.OtelCol.Namespace,
        Labels:    labels,
    },
    Spec: appsv1.DeploymentSpec{
        Replicas: params.OtelCol.Spec.Replicas,
        Selector: &metav1.LabelSelector{
            MatchLabels: selectorLabels,
        },
        Template: corev1.PodTemplateSpec{
            Spec: corev1.PodSpec{
                Containers: []corev1.Container{
                    {
                        Name:  "otc-container",
                        Image: params.OtelCol.Spec.Image,
                    },
                },
            },
        },
    },
}
```

你用的是 Go struct，但最終 controller-runtime 會把它序列化成 JSON 發給 API Server，跟你手寫 YAML 是等價的。

---

## 8.5 `k8s.io/apimachinery`：K8s 通用工具

`api` 套件是資源定義，`apimachinery` 是操作這些資源的工具。

### `apimachinery/pkg/api/errors`：K8s 錯誤判斷

```go
import apierrors "k8s.io/apimachinery/pkg/api/errors"

// internal/controllers/opentelemetrycollector_controller.go:236
if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
    if !apierrors.IsNotFound(err) {
        // 不是 404，是真正的錯誤，要回報
        return ctrl.Result{}, err
    }
    // 是 404：CR 已被刪除，停止 Reconcile
    return ctrl.Result{}, nil
}
```

常用的判斷函式：

| 函式 | 對應 HTTP 狀態 | 用途 |
|---|---|---|
| `IsNotFound(err)` | 404 | 資源不存在 |
| `IsAlreadyExists(err)` | 409 | 建立時物件已存在 |
| `IsConflict(err)` | 409 | Update 時版本衝突（樂觀鎖） |
| `IsForbidden(err)` | 403 | 沒有權限 |
| `IsUnauthorized(err)` | 401 | 未認證 |

### `apimachinery/pkg/api/equality`：語意相等比較

```go
import apiequality "k8s.io/apimachinery/pkg/api/equality"

// internal/manifests/mutate.go:295
if !apiequality.Semantic.DeepEqual(desired.Spec.Selector, existing.Spec.Selector) {
    return &ImmutableFieldChangeErr{Field: "Spec.Selector"}
}
```

`apiequality.Semantic.DeepEqual` 與 Go 標準的 `reflect.DeepEqual` 的差異：

- `reflect.DeepEqual`：純粹比較記憶體值，`nil` 和空 slice 是不同的
- `apiequality.Semantic.DeepEqual`：理解 K8s 語意，`nil` 和空 slice 視為相同，`0` 和未設定的 int 視為相同

這對比較 K8s 資源很重要，因為 API Server 回傳的物件常有預設值填充。

### `apimachinery/pkg/apis/meta/v1`：通用 Metadata

```go
import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// 幾乎每個 K8s 物件都需要
metav1.ObjectMeta{
    Name:      "my-collector",
    Namespace: "default",
    Labels:    map[string]string{"app": "otel"},
}

// Label Selector
metav1.LabelSelector{
    MatchLabels: map[string]string{"app": "otel"},
}
```

---

## 8.6 `github.com/go-logr/logr`：結構化日誌介面

`go-logr/logr` 是一個日誌**介面**，不是具體的日誌實作。OTel Operator 背後使用 `zap` 作為實作，但所有程式碼只依賴 `logr` 介面，讓替換底層日誌庫時不需要改任何業務程式碼。

```go
// internal/controllers/opentelemetrycollector_controller.go
type OpenTelemetryCollectorReconciler struct {
    log logr.Logger   // ← 介面，不是 *zap.Logger
    ...
}

// 使用
log.Info("Starting reconciliation", "name", req.Name, "namespace", req.Namespace)
log.Error(err, "unable to fetch OpenTelemetryCollector")

// 結構化欄位：key-value pair 格式
log.Info("Skipping reconciliation",
    "name", req.String(),
    "reason", "unmanaged",
)

// 加上固定的上下文欄位
childLog := log.WithValues("collector", instance.Name, "namespace", instance.Namespace)
childLog.Info("Processing upgrade")  // 每條 log 都自動帶上 collector 和 namespace
```

**verbosity level：**

```go
log.V(0).Info("重要訊息")  // 預設等級，總是顯示
log.V(1).Info("詳細訊息")  // 需要設定 --zap-log-level=1 才顯示
log.V(2).Info("除錯訊息")  // 開發時使用

// internal/controllers/common.go:161
l.V(1).Info(fmt.Sprintf("desired has been %s", op))  // created/updated/unchanged
```

---

## 8.7 `dario.cat/mergo`：結構體深度合併

`mergo` 解決的問題：如何把一個 struct 的非零值合併進另一個 struct，不覆蓋已存在的欄位。

OTel Operator 用它來實作 Update 時的欄位合併：

```go
// internal/manifests/mutate.go:195-196
func mergeWithOverride(dst, src any) error {
    return mergo.Merge(dst, src, mergo.WithOverride)
}
```

**具體範例：**

```go
type PodSpec struct {
    Resources    ResourceRequirements
    NodeSelector map[string]string
    Tolerations  []Toleration
}

existing := PodSpec{
    Resources:    ResourceRequirements{Limits: existingLimits},
    NodeSelector: map[string]string{"zone": "us-east-1"},
}
desired := PodSpec{
    Resources:    ResourceRequirements{Limits: desiredLimits},
    // NodeSelector 未設定（零值）
}

// WithOverride：src 的非零值覆蓋 dst
mergo.Merge(&existing, desired, mergo.WithOverride)
// 結果：Resources 被更新，NodeSelector 保留（因為 desired 的 NodeSelector 是 nil）
```

**`WithOverride` 的意義：**

| 情況 | 不加 `WithOverride` | 加 `WithOverride` |
|---|---|---|
| src 欄位非零，dst 欄位非零 | 保留 dst 值 | 使用 src 值（覆蓋）|
| src 欄位非零，dst 欄位零值 | 使用 src 值 | 使用 src 值 |
| src 欄位零值 | 保留 dst 值 | 保留 dst 值 |

這讓 `mutate` 函式可以安全地更新需要更新的欄位，同時保留 K8s 自動填入的欄位（如 `clusterIP`、`resourceVersion`）。

---

## 8.8 `github.com/Masterminds/semver`：語意化版本

OTel Operator 需要判斷 Collector 的版本，決定是否需要執行升級遷移邏輯。

```go
// pkg/collector/upgrade/upgrade.go
import semver "github.com/Masterminds/semver/v3"

// 解析版本字串
instanceV, err := semver.NewVersion(otelcol.Status.Version)
// instanceV 是 "0.95.0" → 變成可比較的物件

otelColV, err := semver.NewVersion(u.Version.OpenTelemetryCollector)
// otelColV 是 "0.96.0"

// 比較
if instanceV.LessThan(otelColV) {
    // 需要升級
}
```

```go
// pkg/collector/upgrade/versions.go
// 定義每個需要特殊處理的版本節點
versions := []upgradeVersion{
    {Version: *semver.MustParse("0.19.0"), upgrade: upgradeV0_19_0},
    {Version: *semver.MustParse("0.24.0"), upgrade: upgradeV0_24_0},
    {Version: *semver.MustParse("0.31.0"), upgrade: upgradeV0_31_0},
}
// 若 CR 目前版本是 0.18.0，Operator 版本是 0.31.0，
// 會依序執行 upgradeV0_19_0 → upgradeV0_24_0 → upgradeV0_31_0
```

**`semver.MustParse` vs `semver.NewVersion`：**

- `MustParse`：解析失敗直接 panic，用於硬編碼的版本（必然合法）
- `NewVersion`：回傳 error，用於從外部讀取的版本字串（可能格式錯誤）

---

## 8.9 `sigs.k8s.io/controller-runtime/pkg/controller/controllerutil`：Finalizer 管理

```go
import "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

// internal/controllers/opentelemetrycollector_controller.go

// 加 Finalizer（告訴 K8s「刪除前先等我」）
if controllerutil.AddFinalizer(instance, collectorFinalizer) {
    // AddFinalizer 回傳 true 代表真的加了（之前沒有）
    err = r.Update(ctx, instance)
}

// 確認是否有 Finalizer
if controllerutil.ContainsFinalizer(instance, collectorFinalizer) {
    // 執行清理邏輯（刪除 ClusterRole、ClusterRoleBinding 等 cluster-scoped 資源）
    cleanupClusterResources()

    // 清理完成後移除 Finalizer，讓 K8s 真正刪除物件
    if controllerutil.RemoveFinalizer(instance, collectorFinalizer) {
        err = r.Update(ctx, instance)
    }
}
```

**Finalizer 的完整流程：**

```
使用者 kubectl delete opentelemetrycollector my-col
        │
        ▼
K8s 看到物件有 Finalizer → 不直接刪除
只設定 DeletionTimestamp，物件繼續存在
        │
        ▼
Reconcile 被觸發（因為物件被修改）
        │
        ├── ContainsFinalizer? → 是
        ├── 執行清理邏輯
        ├── RemoveFinalizer
        └── Update 物件
                │
                ▼
        K8s 看到 Finalizer 清空 → 真正刪除物件
```

---

## 8.10 Library 依賴關係圖（完整版）

```
你的 Controller / Webhook
  │
  ├─ ctrl (controller-runtime)
  │    ├─ Manager          ← 生命週期、Leader Election、共用快取
  │    ├─ client.Client    ← CRUD 操作（讀快取，寫 API Server）
  │    ├─ CreateOrUpdate   ← 冪等建立/更新
  │    ├─ SetControllerReference  ← 設定 OwnerReference
  │    ├─ Predicate        ← 事件過濾（第 7 章）
  │    └─ controllerutil   ← Finalizer 管理
  │
  ├─ client-go
  │    ├─ retry            ← 樂觀鎖衝突重試
  │    └─ events           ← K8s Event 記錄
  │
  ├─ k8s.io/api            ← Deployment / Service / Pod 型別定義
  │
  ├─ k8s.io/apimachinery
  │    ├─ api/errors       ← IsNotFound / IsConflict 判斷
  │    ├─ api/equality     ← K8s 語意的深度相等比較
  │    └─ apis/meta/v1     ← ObjectMeta / LabelSelector
  │
  ├─ go-logr/logr          ← 結構化日誌介面（底層是 zap）
  ├─ dario.cat/mergo       ← struct 深度合併（用於 mutate）
  └─ Masterminds/semver    ← 版本比較（用於升級遷移）
```

---

## 練習 8：追蹤真實用法

### 問題 1

`internal/controllers/common.go` 第 143-147 行：

```go
crudErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
    result, createOrUpdateErr := ctrl.CreateOrUpdate(ctx, kubeClient, existing, mutateFn)
    op = result
    return createOrUpdateErr
})
```

**問：** 如果每次 `CreateOrUpdate` 都遇到 `Conflict`，`retry.DefaultRetry` 最多會重試幾次？超過後會怎樣？如何自訂重試次數？

<details>
<summary>參考答案</summary>

`retry.DefaultRetry` 定義如下（來自 `k8s.io/client-go/util/retry`）：

```go
var DefaultRetry = wait.Backoff{
    Steps:    5,        // 最多重試 5 次（加上第 1 次，共 6 次嘗試）
    Duration: 10 * time.Millisecond,
    Factor:   1.0,
    Jitter:   0.1,
}
```

超過 5 次後，`RetryOnConflict` 會回傳最後一次的 Conflict 錯誤。這個錯誤會向上傳遞，Reconcile 會回傳 error，controller-runtime 會自動把這個 Request 重新放進 WorkQueue，稍後再試。

自訂重試：
```go
customBackoff := wait.Backoff{
    Steps:    10,
    Duration: 50 * time.Millisecond,
    Factor:   2.0,  // 指數退避
}
retry.RetryOnConflict(customBackoff, fn)
```
</details>

---

### 問題 2

`internal/manifests/mutate.go` 第 295 行使用了 `apiequality.Semantic.DeepEqual`，而不是 `reflect.DeepEqual`。

**問：** 找一個實際案例，說明如果這裡用 `reflect.DeepEqual`，會造成什麼錯誤行為？（提示：想想 K8s 資源的預設值）

<details>
<summary>參考答案</summary>

**範例：`Deployment.Spec.Selector.MatchExpressions`**

當你建立的 Deployment 沒有設定 `MatchExpressions` 時：
- **你的 desired**：`MatchExpressions: nil`
- **API Server 回傳的 existing**：`MatchExpressions: []LabelSelectorRequirement{}`（空 slice，不是 nil）

用 `reflect.DeepEqual`：`nil != []{}` → 判斷為「不相等」→ 誤以為 Selector 被改了 → 回傳 `ImmutableFieldChangeErr` → 嘗試刪除並重建 Deployment → 服務中斷

用 `apiequality.Semantic.DeepEqual`：`nil == []{}` → 判斷為「相等」→ 正確忽略這個差異
</details>

---

### 問題 3（動手）

在 `pkg/collector/upgrade/versions.go` 中，每個版本節點對應一個 `upgrade` 函式。

選任意一個版本（如 `v0_19_0`），找到對應的函式，說明它做了什麼遷移，以及為什麼這個遷移需要用程式碼處理，而不能只靠 Reconcile 的正常邏輯完成。
</details>
