# 第 1 章：Kubernetes Operator Pattern

> **你已知道：** Pod、Deployment、Service 等內建資源  
> **本章目標：** 理解 Operator 的核心思想，以及 controller-runtime 的運作方式

---

## 1.1 為什麼需要 Operator？

Kubernetes 內建資源（Deployment、Service...）只能管理**通用的工作負載**。但有些系統有複雜的**運維知識**，例如：

- OTel Collector 需要根據 config YAML 自動開放對應的 port
- 多個 Collector 副本需要有人分配 Prometheus scrape targets
- 升級 Collector 版本時需要特殊的遷移邏輯

這些知識沒辦法用 Deployment YAML 表達。**Operator 讓你用程式碼把運維知識編碼進 Kubernetes**。

### 命令式 vs 聲明式

```
命令式（你說步驟）：
  幫我建一個 OTel Collector，然後建 Service，然後設定 ConfigMap...

聲明式（你說期望結果）：
  我希望有一個這樣的 OpenTelemetryCollector：
    mode: deployment
    replicas: 2
    config: ...
  （Operator 負責讓 cluster 達到這個狀態）
```

---

## 1.2 控制迴圈（Control Loop）

Operator 的核心機制是一個永不停止的迴圈：

```
┌──────────────────────────────────────────┐
│                                          │
│   觀察（Observe）                         │
│   CR 目前的 Spec 是什麼？                 │
│   Cluster 目前的狀態是什麼？               │
│              ↓                           │
│   差距分析（Diff）                         │
│   期望狀態 vs 實際狀態 有何不同？           │
│              ↓                           │
│   行動（Act）                             │
│   Create / Update / Delete 資源           │
│              ↓                           │
│   （回到觀察）←──────────────────────────  │
└──────────────────────────────────────────┘
```

這個迴圈在程式中就是 `Reconcile()` 函式。每當 CR 或其管理的子資源有任何變化，`Reconcile()` 就會被呼叫一次。

### 重要特性：冪等性（Idempotency）

`Reconcile()` 必須能被**重複呼叫多次，結果都相同**。原因：

- k8s 不保證只呼叫一次
- 網路錯誤後會重試
- Operator 重啟後會重新對所有 CR 執行 Reconcile

這意味著邏輯應該是「讓 cluster 達到期望狀態」，而不是「執行某個步驟」。

---

## 1.3 三個核心概念對應到程式碼

### CRD（Custom Resource Definition）

讓 Kubernetes 認識新的資源類型。就像 Kubernetes 原本認識 `Deployment`，透過 CRD 讓它也認識 `OpenTelemetryCollector`。

```
定義在：apis/v1beta1/opentelemetrycollector_types.go
        apis/v1alpha1/instrumentation_types.go
```

### Controller（控制器）

監聽 CR 的變化，執行 `Reconcile()` 迴圈。

```
定義在：internal/controllers/opentelemetrycollector_controller.go:231
```

### Webhook（准入控制）

攔截 Pod 建立請求，在 Pod 實際建立前修改它（加入 OTel agent）。

```
定義在：internal/webhook/podmutation/webhookhandler.go:60
```

---

## 1.4 看懂 `Reconcile()` 的骨架

打開 [`internal/controllers/opentelemetrycollector_controller.go`](../internal/controllers/opentelemetrycollector_controller.go)，看第 231-293 行：

```go
func (r *OpenTelemetryCollectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

    // 步驟 1：從 k8s 讀取 CR（期望狀態）
    var instance v1beta1.OpenTelemetryCollector
    if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
        // 404 = 已被刪除，忽略（GC 會處理子資源）
    }

    // 步驟 2：特殊情況處理
    if instance.Spec.ManagementState == v1beta1.ManagementStateUnmanaged {
        return ctrl.Result{}, nil  // 使用者說不要管這個 CR
    }

    // 步驟 3：計算「應該有哪些 k8s 資源」
    desiredObjects, _ := BuildCollector(params)

    // 步驟 4：查詢「cluster 目前實際有哪些資源」
    ownedObjects, _ := r.findOtelOwnedObjects(ctx, params)

    // 步驟 5：讓現實符合期望（Create/Update/Delete）
    err = reconcileDesiredObjects(ctx, r.Client, log, &instance, params.Scheme, desiredObjects, ownedObjects)

    // 步驟 6：更新 CR 的 Status（讓使用者看到目前狀態）
    return collectorStatus.HandleReconcileStatus(ctx, log, params, instance, err)
}
```

**`ctrl.Result` 的意義：**
- `ctrl.Result{}` — 成功，等待下次事件再 Reconcile
- `ctrl.Result{Requeue: true}` — 要求立刻重新 Reconcile
- `ctrl.Result{RequeueAfter: 30s}` — 30 秒後重新 Reconcile

---

## 1.5 `SetupWithManager()` — 告訴 k8s 監聽什麼

```go
// internal/controllers/opentelemetrycollector_controller.go:297
func (r *OpenTelemetryCollectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1beta1.OpenTelemetryCollector{}).  // 監聽這種 CR
        Owns(&appsv1.Deployment{}).              // 也監聽它建立的子資源
        Owns(&corev1.Service{}).
        // ...
        Complete(r)
}
```

當 `OpenTelemetryCollector` **或它建立的任何子資源**有變化，`Reconcile()` 都會被觸發。

**例子：** 有人手動刪了 Operator 建立的 Deployment → Operator 偵測到 → 重新建立它

---

## 練習 1：閱讀理解

打開 [`internal/controllers/opentelemetrycollector_controller.go`](../internal/controllers/opentelemetrycollector_controller.go)，回答以下問題：

### 問題 1
第 265-272 行有一個 upgrade 相關的邏輯：

```go
if r.upgrade.NeedsUpgrade(instance) {
    err = r.upgrade.Upgrade(ctx, instance)
    return ctrl.Result{Requeue: true, RequeueAfter: 1 * time.Second}, nil
}
```

**問：** 為什麼 upgrade 之後要 `Requeue: true`？如果不 requeue 會發生什麼？

<details>
<summary>參考答案</summary>

Upgrade 會修改 CR 的內容（例如更新版本號或 annotation）。修改後需要重新從最新的 CR 狀態開始 Reconcile，確保後續步驟（BuildCollector、reconcileDesiredObjects）使用的是升級後的 spec。

如果不 requeue，當次 Reconcile 用的是升級前讀取的舊 instance，可能產生錯誤或不一致。
</details>

### 問題 2
第 275-279 行：

```go
if maybeAddFinalizer(params, &instance) {
    err = r.Update(ctx, &instance)
    return ctrl.Result{}, err
}
```

**問：** `Finalizer` 是什麼？為什麼加了 Finalizer 之後要提前 return？

<details>
<summary>參考答案</summary>

Finalizer 是一個標記，告訴 k8s「在真正刪除這個物件前，先等 Operator 執行清理邏輯」。這裡用來確保 ClusterRole/ClusterRoleBinding（cluster-scoped，沒有 ownerReference）在 CR 刪除時也會被清理。

加了 Finalizer 後 `Update` 會觸發新的 Reconcile 事件，所以提前 return 避免重複執行後續邏輯。
</details>

### 問題 3（動手）

`SetupWithManager()` 在第 297 行。找看看，`GetOwnedResourceTypes()` 回傳哪些類型？

列出至少 5 種，並說明為什麼 Operator 需要 `Owns()` 這些資源。

---

## 練習 2：在本地觀察 Reconcile

如果你有 Kubernetes cluster（或 kind），可以嘗試：

```bash
# 安裝 CRD
make install

# 本地執行 operator（會顯示 log）
make run

# 另一個 terminal，建立一個 CR
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: my-collector
spec:
  mode: deployment
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
EOF

# 觀察 operator log 中的 Reconcile 訊息
# 觀察建立了哪些 k8s 資源
kubectl get all -l app.kubernetes.io/managed-by=opentelemetry-operator
```

---

[← 回目錄](./00-overview.md) | [第 2 章：CRD 資料模型 →](./02-crd-api-types.md)
