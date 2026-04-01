# 第 5 章：Reconcile 迴圈的完整實現

> **本章目標：** 深入理解 Create / Update / Delete 的完整實現，以及各種邊界情況的處理

---

## 5.1 從 Reconcile() 到 reconcileDesiredObjects()

上一章看了 `Reconcile()` 的骨架，這章深入最後一步：

```go
// internal/controllers/opentelemetrycollector_controller.go:282-293

desiredObjects, _ := BuildCollector(params)      // 「應該有什麼」
ownedObjects, _  := r.findOtelOwnedObjects(...)  // 「目前有什麼」

// 讓現實符合期望
err = reconcileDesiredObjects(
    ctx, r.Client, log,
    &instance,      // owner（CR）
    params.Scheme,
    desiredObjects, // 期望狀態
    ownedObjects,   // 目前狀態（map[UID]Object）
)
```

`ownedObjects` 是一個 `map[types.UID]client.Object`：
- **Key**：k8s 資源的 UID（唯一識別碼）
- **Value**：資源物件本身

---

## 5.2 reconcileDesiredObjects() 完整解析

**檔案：** [`internal/controllers/common.go`](../internal/controllers/common.go)（第 124 行）

整個函式只做三件事，邏輯非常精簡：

```go
func reconcileDesiredObjects(
    ctx context.Context,
    kubeClient client.Client,
    logger logr.Logger,
    owner metav1.Object,
    scheme *runtime.Scheme,
    desiredObjects []client.Object,    // 「應該有」的資源
    ownedObjects map[types.UID]client.Object,  // 「目前有」的資源
) error {
    var errs []error

    // ① 對每個「期望存在」的資源，執行 CreateOrUpdate
    for _, desired := range desiredObjects {
        // 設定 OwnerReference（讓 GC 機制知道這個資源屬於誰）
        if isNamespaceScoped(desired) {
            ctrl.SetControllerReference(owner, desired, scheme)
        }

        existing := desired.DeepCopyObject().(client.Object)
        mutateFn := manifests.MutateFuncFor(existing, desired)

        // 有衝突時自動重試
        crudErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
            result, err := ctrl.CreateOrUpdate(ctx, kubeClient, existing, mutateFn)
            op = result  // "created", "updated", "unchanged"
            return err
        })

        // 特殊情況：不可變欄位變更
        if errors.As(crudErr, &manifests.ImmutableChangeErr) {
            kubeClient.Delete(ctx, existing)
            continue  // 下次 Reconcile 時會重新建立
        }

        // 成功 → 從「待刪除」名單移除
        delete(ownedObjects, existing.GetUID())
    }

    // ② 刪除多餘資源（ownedObjects 裡剩下的都是「不該存在的」）
    err := deleteObjects(ctx, kubeClient, logger, ownedObjects)
    return err
}
```

---

## 5.3 Map 做 Pruning 的精妙設計

這個演算法用一個 map 完成了「找出要刪除的資源」，不需要比較兩個列表：

```
初始狀態：
  ownedObjects = { uid-A: DeploymentA, uid-B: ServiceB, uid-C: ServiceC }
  （cluster 目前有三個資源）

  desiredObjects = [ Deployment(新), Service(新) ]
  （期望有兩個資源）

Step 1: 處理 DeploymentA
  CreateOrUpdate → 更新成功
  delete(ownedObjects, uid-A)
  ownedObjects = { uid-B: ServiceB, uid-C: ServiceC }

Step 2: 處理 Service(新)
  CreateOrUpdate → 更新成功（更新 uid-B）
  delete(ownedObjects, uid-B)
  ownedObjects = { uid-C: ServiceC }

Step 3: 迴圈結束
  ownedObjects = { uid-C: ServiceC }  ← 剩下的就是「多餘的」

Step 4: deleteObjects
  刪除 ServiceC
```

這個設計的優雅之處：**不需要比較兩個列表，只需要一次遍歷**。

---

## 5.4 ctrl.CreateOrUpdate() 的運作原理

這是 controller-runtime 提供的輔助函式，簡化了 Create/Update 的邏輯：

```go
// 偽代碼說明
func CreateOrUpdate(ctx, client, existing, mutateFn) (OperationResult, error) {
    // 1. 嘗試從 k8s 讀取這個資源（用 existing 的 name/namespace）
    err := client.Get(ctx, objectKey(existing), existing)

    if isNotFound(err) {
        // 資源不存在 → 呼叫 mutateFn 填入期望的值，然後 Create
        mutateFn()
        err = client.Create(ctx, existing)
        return OperationResultCreated, err
    }

    // 資源存在 → 呼叫 mutateFn 修改 existing，然後 Update
    // （mutateFn 在這裡才被呼叫，此時 existing 已有 k8s 填入的欄位）
    mutateFn()
    err = client.Update(ctx, existing)
    return OperationResultUpdated, err
}
```

**重要：** `mutateFn` 是在 `existing` 已從 k8s 讀回之後才執行。所以 `existing` 裡有 `ResourceVersion`、`ClusterIP` 等 k8s 自動填的欄位，`mutateFn` 只更新需要更新的欄位，不會破壞這些欄位。

---

## 5.5 MutateFuncFor() — 精確控制更新哪些欄位

**檔案：** [`internal/manifests/mutate.go`](../internal/manifests/mutate.go)（第 59 行）

每種資源類型都有各自的 mutate 邏輯，控制哪些欄位需要更新：

### Labels / Annotations：合併而非覆蓋

```go
// 第 63-75 行
existingAnnotations := existing.GetAnnotations()
mergeWithOverride(&existingAnnotations, desired.GetAnnotations())
// mergeWithOverride 使用 mergo.Merge with Override 語意：
// - desired 的值覆蓋 existing 的同名 key
// - existing 有但 desired 沒有的 key → 保留

existing.SetAnnotations(existingAnnotations)
```

這讓使用者可以手動在資源上加 annotation（例如 Datadog 或其他工具加的），Operator 不會把它刪掉。

### Service：只更新核心欄位

```go
func mutateService(existing, desired *corev1.Service) {
    existing.Spec.Ports    = desired.Spec.Ports
    existing.Spec.Selector = desired.Spec.Selector
    // 注意：不動 ClusterIP、LoadBalancerIP 等自動分配的欄位
}
```

如果把整個 `existing.Spec = desired.Spec`，`ClusterIP` 會變成空字串，導致 k8s 回傳錯誤（immutable field）。

### Deployment：更多欄位，但 Selector 不能動

```go
func mutateDeployment(existing, desired *appsv1.Deployment) error {
    // 先檢查 Selector 有沒有變（不可變欄位）
    if !apiequality.Semantic.DeepEqual(desired.Spec.Selector, existing.Spec.Selector) {
        return &ImmutableFieldChangeErr{Field: "Spec.Selector"}
    }

    // 安全地更新這些欄位
    existing.Spec.Replicas             = desired.Spec.Replicas
    existing.Spec.Strategy             = desired.Spec.Strategy
    existing.Spec.RevisionHistoryLimit = desired.Spec.RevisionHistoryLimit

    // PodTemplate 需要特殊的 merge 邏輯
    return mutatePodTemplate(&existing.Spec.Template, &desired.Spec.Template)
}
```

### ConfigMap：直接覆蓋資料

```go
func mutateConfigMap(existing, desired *corev1.ConfigMap) {
    existing.BinaryData = desired.BinaryData
    existing.Data       = desired.Data
    // ConfigMap 沒有自動填的欄位，直接覆蓋即可
}
```

### Ingress：刻意不更新 Labels/Annotations

```go
func mutateIngress(existing, desired *networkingv1.Ingress) {
    // 注意：沒有更新 Labels 和 Annotations！
    // 見 issue #4322：某些 ingress controller 會修改 Ingress 的 annotation，
    // 如果 Operator 每次都覆蓋，會導致 ingress controller 的設定被清掉
    existing.Spec.DefaultBackend = desired.Spec.DefaultBackend
    existing.Spec.Rules          = desired.Spec.Rules
    existing.Spec.TLS            = desired.Spec.TLS
}
```

這是個有趣的設計決策：為了與其他 controller 和平共存，刻意讓 Labels/Annotations 不被管理。

---

## 5.6 不可變欄位的處理

有些 k8s 資源的欄位建立後就不能修改，例如：
- `Deployment.Spec.Selector`
- `StatefulSet.Spec.Selector`
- `StatefulSet.Spec.VolumeClaimTemplates`
- `StatefulSet.Spec.ServiceName`

當使用者改了這些欄位，直接 Update 會被 k8s 拒絕。Operator 的處理方式：

```go
// common.go:148-153
if errors.As(crudErr, &manifests.ImmutableChangeErr) {
    l.Error(crudErr, "detected immutable field change, trying to delete, new object will be created on next reconcile")
    delErr := kubeClient.Delete(ctx, existing)
    // 刪除後，下次 Reconcile 會重新 Create
    continue
}
```

**流程：**
1. 使用者修改 CR 中的 `Spec.Selector` 相關欄位
2. Reconcile 觸發 → `mutateDeployment()` 偵測到 Selector 變了 → 回傳 `ImmutableChangeErr`
3. Operator 刪除舊的 Deployment
4. 重新 Reconcile（因為 Delete 事件觸發）→ CreateOrUpdate → 建立新的 Deployment

**使用者體驗：** 短暫的停機（舊 Deployment 刪除到新 Deployment ready 之間）。

---

## 5.7 OwnerReference 與 Garbage Collection

```go
// common.go:132
ctrl.SetControllerReference(owner, desired, scheme)
```

這讓每個子資源都帶有 ownerReference：

```yaml
ownerReferences:
  - apiVersion: opentelemetry.io/v1beta1
    kind: OpenTelemetryCollector
    name: my-collector
    uid: abc-123-def
    controller: true
    blockOwnerDeletion: true
```

**效果：** 當 `OpenTelemetryCollector` CR 被刪除時，k8s 的 GC 會自動刪除所有帶這個 ownerReference 的子資源（Deployment、Service、ConfigMap...）。

**Operator 不需要手動清理命名空間內的資源。**

例外：`ClusterRole` 和 `ClusterRoleBinding` 是 cluster-scoped（不屬於任何 namespace），**無法設定 ownerReference**。所以 Operator 使用 **Finalizer** 機制手動清理它們（見 `controller.go:370-412`）。

---

## 5.8 衝突重試機制

```go
// common.go:143-147
crudErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
    result, createOrUpdateErr := ctrl.CreateOrUpdate(ctx, kubeClient, existing, mutateFn)
    op = result
    return createOrUpdateErr
})
```

**為什麼需要重試？**

當多個 controller 同時修改同一個資源，k8s 用 `ResourceVersion` 做樂觀鎖。如果讀到的版本跟 Update 時的版本不一致，k8s 回傳 `Conflict` 錯誤。

`retry.RetryOnConflict` 在遇到 Conflict 時，重新讀取最新版本再試一次，預設重試 5 次。

---

## 5.9 完整流程圖

```
CR 建立 / 更新 / 刪除 / 子資源變化
              │
              ▼
         Reconcile()
              │
    ┌─────────┴──────────┐
    │  r.Get(CR)          │← 404 則 return（GC 處理）
    └─────────┬──────────┘
              │
    ┌─────────┴──────────┐
    │  BuildCollector()   │← 純計算，無 API 呼叫
    │  → desiredObjects   │
    └─────────┬──────────┘
              │
    ┌─────────┴──────────┐
    │ findOtelOwnedObjects│← 查詢 cluster 現有資源
    │ → ownedObjects map  │
    └─────────┬──────────┘
              │
    ┌─────────┴──────────────────────────────┐
    │  reconcileDesiredObjects()              │
    │                                         │
    │  for each desired:                      │
    │    SetControllerReference               │
    │    CreateOrUpdate (with retry)          │
    │    ├─ 不存在 → Create                  │
    │    ├─ 存在   → MutateFuncFor → Update  │
    │    └─ Immutable 變了 → Delete           │
    │    delete(ownedObjects, uid)            │
    │                                         │
    │  deleteObjects(剩下的 ownedObjects)     │← Pruning
    └─────────────────────────────────────────┘
              │
              ▼
    HandleReconcileStatus()  ← 更新 CR 的 Status
```

---

## 練習 1：閱讀理解

**問題 1：** 打開 [`internal/controllers/common.go`](../internal/controllers/common.go)，找到 `isNamespaceScoped()` 函式（第 29 行）。

為什麼 `ClusterRole` 和 `ClusterRoleBinding` 不是 namespace-scoped？這對 `SetControllerReference` 有什麼影響？

<details>
<summary>參考答案</summary>

`ClusterRole` 和 `ClusterRoleBinding` 是 cluster 層級的資源，不屬於任何 namespace。`SetControllerReference` 要求 owner 和 owned 物件在同一個 namespace，對 cluster-scoped 物件設定 ownerReference 會導致錯誤。

所以這些資源不設定 ownerReference，改用 Finalizer 手動清理（見 `finalizeCollector()` 第 372 行）。
</details>

**問題 2：** 打開 [`internal/manifests/mutate.go`](../internal/manifests/mutate.go)，找到 `mutateStatefulSet()`（第 330 行）。

StatefulSet 的 `VolumeClaimTemplates` 為什麼是不可變的？這在使用上有什麼限制？

<details>
<summary>參考答案</summary>

`VolumeClaimTemplates` 定義了 StatefulSet 中每個 Pod 的 PVC 模板。k8s 在 StatefulSet 建立後就把這些 PVC 固定下來（每個 Pod 一個 PVC），因為 PVC 代表實際的儲存，改變模板不能自動遷移資料。

使用限制：如果需要修改 `VolumeClaimTemplates`（例如擴大儲存容量），必須刪除 StatefulSet（資料保留，因為 PVC 獨立存在）再重建。這會觸發 Operator 的 delete-and-recreate 流程。
</details>

---

## 練習 2：撰寫單元測試

仿照 [`internal/controllers/reconcile_test.go`](../internal/controllers/reconcile_test.go) 的模式，試著理解並撰寫一個測試：

**目標：** 測試當 `desiredObjects` 為空時（例如 CR 被設為 unmanaged），`reconcileDesiredObjects()` 會刪除所有 `ownedObjects`。

```go
// 提示：這個函式是 package-private，需要在同 package 內測試
// 或者透過 Controller 層面測試

func TestReconcileDesiredObjects_PrunesOrphanedResources(t *testing.T) {
    // 建立一個假的 k8s client
    // 建立一些 existing 資源（模擬 ownedObjects）
    // 呼叫 reconcileDesiredObjects(ctx, client, log, owner, scheme,
    //   []client.Object{},  // 空的 desired
    //   ownedObjects,
    // )
    // 驗證 client 收到了 Delete 呼叫
}
```

查看 `internal/controllers/reconcile_test.go` 是否有類似的測試可以參考。

---

## 練習 3：追蹤 ConfigMap 的更新流程

當使用者修改了 `OpenTelemetryCollector.Spec.Config`，追蹤完整的更新流程：

1. **CR 更新** → Reconcile 觸發
2. `BuildCollector()` → `ConfigMap()` 被呼叫
   - 新的 Config 內容 → 產生新的 SHA hash
   - ConfigMap 名稱改變（包含新 hash）
3. `reconcileDesiredObjects()`：
   - 新的 ConfigMap（不同名稱）→ **Create**（不是 Update）
   - 舊的 ConfigMap 不在 desiredObjects 裡 → 留在 ownedObjects

**問題：** 舊的 ConfigMap 會立刻被刪除嗎？

看 [`internal/controllers/opentelemetrycollector_controller.go`](../internal/controllers/opentelemetrycollector_controller.go) 的 `findOtelOwnedObjects()` 和 `getCollectorConfigMapsToKeep()` 函式（第 78-154 行），理解 `ConfigVersions` 欄位的作用。

<details>
<summary>參考答案</summary>

不會立刻刪除。`getCollectorConfigMapsToKeep()` 會保留最新的 `ConfigVersions + 1` 個 ConfigMap（預設 3 + 1 = 4 個），讓使用者有時間回滾。

流程：
1. `findOtelOwnedObjects()` 查出所有屬於這個 CR 的 ConfigMap
2. `getCollectorConfigMapsToKeep()` 按建立時間排序，保留最新的 N 個
3. 被保留的 ConfigMap 從 `ownedObjects` 移除（不會被 Pruning 刪除）
4. 只有超過保留數量的舊 ConfigMap 才會被刪除

下一次 Reconcile（如果 Config 沒有再次改變），就不會有新 ConfigMap，此時 ownedObjects 裡的舊 ConfigMap 超過保留數量的會被刪除。
</details>

---

## 總結：五章學習路徑回顧

```
第 1 章：Operator Pattern
  └─ 聲明式、控制迴圈、冪等性

第 2 章：CRD 資料模型
  └─ Spec/Status 分離、kubebuilder markers、四個 CR

第 3 章：Manifest 生成（Spec → k8s 資源）
  └─ 工廠模式、純函式、SHA 命名的 ConfigMap

第 4 章：Webhook 注入
  └─ Admission Webhook、Annotation 解析、initContainer 巧思

第 5 章：Reconcile 迴圈（本章）
  └─ Map Pruning、MutateFuncFor、OwnerReference GC、不可變欄位處理
```

**下一步建議：**

1. **閱讀 TargetAllocator Controller**：[`internal/controllers/targetallocator_controller.go`](../internal/controllers/targetallocator_controller.go)，結構與 Collector Controller 相同，可以驗證你的理解
2. **閱讀 Upgrade 機制**：[`pkg/collector/upgrade/`](../pkg/collector/upgrade/)，了解版本升級時如何遷移 CR
3. **執行完整測試**：`make test`，觀察測試結構，嘗試為你想理解的函式寫測試

---

[← 第 4 章：Webhook 注入](./04-webhook-injection.md) | [← 回目錄](./00-overview.md)
