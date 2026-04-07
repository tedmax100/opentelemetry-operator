# 第 7 章：Operator 怎麼知道要 Reconcile？Watch 機制與 Informer 內部

> **本章目標：** 理解 Kubernetes 通知 Operator 的底層管線，以及 OTel Operator 如何精確訂閱自己關心的資源

---

## 7.1 從一個問題開始

你已經知道「有變化就觸發 `Reconcile()`」，但這句話掩蓋了一個核心問題：

> **API Server 怎麼把事件「推」給 Operator？用的是什麼協議？**

答案不是 WebSocket，也不是 gRPC stream，而是一個你可能想不到的技術：**帶有 `?watch=true` 參數的普通 HTTP 請求**。

---

## 7.2 HTTP Watch：長連接的訂閱線路

Kubernetes API Server 提供標準 RESTful API，但它支援一個特殊模式：

```
GET /apis/opentelemetry.io/v1beta1/opentelemetrycollectors?watch=true&resourceVersion=12345
```

這條請求發出後，**連接不會關閉**。API Server 會保持這條線路開啟，每當有事件就發送一個資料塊（chunk）：

```
HTTP/2 200 OK
Transfer-Encoding: chunked
Content-Type: application/json

{"type":"MODIFIED","object":{"kind":"OpenTelemetryCollector","metadata":{"resourceVersion":"12346",...},"spec":{...}}}
{"type":"DELETED","object":{"kind":"OpenTelemetryCollector","metadata":{"resourceVersion":"12347",...}}}
```

### 傳輸協議細節

| 協議 | 機制 | 說明 |
|---|---|---|
| **HTTP/1.1** | Chunked Transfer Encoding | Server 保持連接，每個事件是一個 chunk |
| **HTTP/2**（現代 K8s 預設）| Stream multiplexing | 原生長連接支援，效率更高 |

```
Operator                      API Server
   │                              │
   │── GET /...?watch=true ──────>│  （一次請求）
   │                              │
   │<── HTTP 200 (連接保持) ───────│
   │<── chunk: MODIFIED event ────│  （物件被修改）
   │<── chunk: CREATED event ─────│  （新物件建立）
   │<── chunk: DELETED event ─────│  （物件被刪除）
   │           ...                │  （連接持續開啟）
```

> **為什麼不用 WebSocket？**
> HTTP Watch 的優點是：任何標準 HTTP 客戶端（甚至 `curl`）都能用，且 HTTP/2 已原生支援多路復用，透過標準負載平衡器也更好管理。

---

## 7.3 resourceVersion：事件的序號

每個 K8s 物件都有一個 `resourceVersion` 欄位，它是 API Server 維護的**單調遞增序號**，代表「這個物件在整個 cluster 歷史中的版本位置」。

Watch 請求帶著 `resourceVersion=12345` 的意思是：

> 「請把版本 12345 **之後**發生的所有事件傳給我。」

這讓 Operator 在重啟或斷線後可以**從中斷的地方繼續**，不漏掉任何事件，也不需要重新拉取全部資料。

```
時間軸：
  版本 12340  建立了 collector-A
  版本 12343  修改了 collector-A 的 replicas
  版本 12345  ← Operator 上次記錄的位置
  版本 12346  collector-B 被刪除      ← Operator 重啟後從這裡繼續
  版本 12350  建立了 collector-C
```

---

## 7.4 Informer 的三層管線

`client-go` 內部把 Watch 機制包裝成一個三層管線，Operator 不直接操作 HTTP，而是與這個管線互動：

```
┌──────────────────────────────────────────────────────────┐
│                      Informer 內部                        │
│                                                          │
│  ┌─────────────┐    ┌────────────────┐    ┌───────────┐  │
│  │  Reflector  │───>│  DeltaFIFO     │───>│  Indexer  │  │
│  │             │    │  (事件佇列)     │    │  (本地快取)│  │
│  │ 負責打電話   │    │                │    │           │  │
│  │ ?watch=true │    │ ADDED          │    │ 可按 key  │  │
│  │             │    │ MODIFIED       │    │ 查詢物件   │  │
│  │ 自動重連    │    │ DELETED        │    │           │  │
│  └─────────────┘    │ SYNC           │    └───────────┘  │
│                     └────────────────┘                   │
└──────────────────────────────────────────────────────────┘
         │                    │
         │                    ▼
         │          ResourceEventHandler
         │          OnAdd / OnUpdate / OnDelete
         │                    │
         ▼                    ▼
    API Server          WorkQueue
                        (等待 Reconcile 的請求)
```

### 每一層的職責

**Reflector**：負責與 API Server 溝通
1. 啟動時先做 **List**：拿到所有物件的當前快照，記下 `resourceVersion`
2. 接著發起 **Watch**：從剛才的版本號開始，持續接收事件
3. 如果連線斷掉，自動用最後記錄的 `resourceVersion` 重新建立連線

**DeltaFIFO**：事件緩衝佇列
- 按順序存放所有差異（Delta），型別為 `ADDED`、`MODIFIED`、`DELETED`、`SYNC`
- 確保事件不丟失，即使 Reconciler 來不及處理

**Indexer**：本地物件快取
- 儲存所有 Watch 到的物件的最新狀態
- 支援按 label、namespace、name 快速查詢
- Operator 在 `Reconcile()` 裡用 `r.Get()` 讀取的資料，**實際上來自這個本地快取**，而不是每次都打 API Server

---

## 7.5 API Server 的精確過濾：誰負責篩選？

Watch 是「訂閱」，但不是廣播。API Server 在服務端就把資料過濾好了，才傳給 Operator。

### 第一層：URL 路徑過濾

URL 路徑本身就決定了你訂閱的範圍：

```
# 只看特定 namespace 的 OpenTelemetryCollector
GET /apis/opentelemetry.io/v1beta1/namespaces/default/opentelemetrycollectors?watch=true

# 看所有 namespace 的 OpenTelemetryCollector（去掉 namespace 段）
GET /apis/opentelemetry.io/v1beta1/opentelemetrycollectors?watch=true

# 這個 URL 不會有任何 Pod 或 Service 的事件
```

### 第二層：Selector 過濾

可以加上查詢參數讓 API Server 進一步篩選：

```
# 只看帶有特定 label 的物件
?labelSelector=app.kubernetes.io/managed-by%3Dopentelemetry-operator

# 只看特定名稱的物件
?fieldSelector=metadata.name%3Dmy-collector
```

> 這些過濾都在 **API Server 內部**完成，符合條件的事件才會序列化並通過網路傳送。

### 過濾層級總覽

| 過濾層 | 負責方 | 節省的資源 |
|---|---|---|
| URL 路徑（資源種類 + namespace）| API Server | 網路頻寬 |
| Label Selector / Field Selector | API Server | 網路頻寬 |
| Predicate | Operator 本地 | CPU（避免觸發不必要的 Reconcile）|

---

## 7.6 Predicate：客戶端的二次過濾

即使 API Server 已經過濾了大部分資料，有時「物件有變化」並不代表「需要 Reconcile」。

**典型場景：** 物件的 `status.lastHeartbeatTime` 每分鐘更新一次，但你的 Reconciler 只關心 `spec` 有沒有變。如果每次 status 更新都觸發 Reconcile，是純粹的浪費。

這就是 **Predicate** 存在的原因：在事件進入 WorkQueue 前，由 `controller-runtime` 在 Operator 本地再過濾一次。

### OTel Operator 中的真實 Predicate

打開 `internal/controllers/targetallocator_controller.go`，第 209-237 行可以看到兩個 Predicate 的實際應用：

```go
// internal/controllers/targetallocator_controller.go:209-218

// 只監控「有啟用 TargetAllocator」的 OpenTelemetryCollector
ctrlBuilder.Watches(
    &v1beta1.OpenTelemetryCollector{},
    handler.EnqueueRequestsFromMapFunc(getTargetAllocatorForCollector),
    builder.WithPredicates(
        predicate.NewPredicateFuncs(func(object client.Object) bool {
            collector := object.(*v1beta1.OpenTelemetryCollector)
            return collector.Spec.TargetAllocator.Enabled  // ← 只關心這個欄位
        }),
    ),
)
```

```go
// internal/controllers/targetallocator_controller.go:220-237

// 只監控帶有特定 label 的 OpenTelemetryCollector
collectorSelector := metav1.LabelSelector{
    MatchExpressions: []metav1.LabelSelectorRequirement{
        {
            Key:      constants.LabelTargetAllocator,
            Operator: metav1.LabelSelectorOpExists,  // label 存在即符合
        },
    },
}
selectorPredicate, err := predicate.LabelSelectorPredicate(collectorSelector)

ctrlBuilder.Watches(
    &v1beta1.OpenTelemetryCollector{},
    handler.EnqueueRequestsFromMapFunc(getTargetAllocatorRequestsFromLabel),
    builder.WithPredicates(selectorPredicate),  // ← 只有符合 label 的物件才觸發
)
```

這兩個 Watches 說明了一個重要設計：**TargetAllocator Controller 不只監控 `TargetAllocator` CR，它也監控 `OpenTelemetryCollector` CR**，但只有在特定條件下才做出反應。

### controller-runtime 內建的 Predicate

```go
// 常用的內建 Predicate
predicate.GenerationChangedPredicate{}   // 只在 spec 改變時觸發（忽略 status 更新）
predicate.LabelChangedPredicate{}        // 只在 label 改變時觸發
predicate.AnnotationChangedPredicate{}   // 只在 annotation 改變時觸發
predicate.ResourceVersionChangedPredicate{} // 任何版本變化都觸發（預設行為）
```

---

## 7.7 Owns() 的運作原理：OwnerReference 追蹤

第 1 章提過 `Owns()`，現在可以更深入理解它：

```go
// internal/controllers/opentelemetrycollector_controller.go:304-309
builder := ctrl.NewControllerManagedBy(mgr).
    For(&v1beta1.OpenTelemetryCollector{}).
    Owns(&appsv1.Deployment{}).
    Owns(&corev1.Service{}).
    // ...
```

`Owns()` 並不是「監控所有 Deployment」，而是「監控**帶有指向此 CR 的 OwnerReference** 的 Deployment」。

當 Operator 建立子資源時，會在子資源上設定 `OwnerReference`：

```yaml
# 建立出來的 Deployment 長這樣
metadata:
  ownerReferences:
    - apiVersion: opentelemetry.io/v1beta1
      kind: OpenTelemetryCollector
      name: my-collector
      uid: abc-123
      controller: true
```

當這個 Deployment 被刪除或修改，`controller-runtime` 看到 `OwnerReference`，會找到對應的 `OpenTelemetryCollector`，然後把它的 `name/namespace` 放進 WorkQueue，觸發 Reconcile。

**實際效果：** 有人手動刪了 Operator 建立的 Deployment → Operator 偵測到 → 重新建立它。

---

## 7.8 完整事件流程圖

從你執行 `kubectl apply` 到 `Reconcile()` 被呼叫，完整路徑如下：

```
kubectl apply -f my-collector.yaml
         │
         ▼
  API Server (etcd 寫入)
  resourceVersion: 12346
         │
         │  （HTTP Watch 長連接，chunk 推送）
         ▼
  Reflector (client-go)
  收到 MODIFIED 事件
         │
         ▼
  DeltaFIFO
  存入 Delta{Type: Modified, Object: ...}
         │
         ▼
  Predicate 過濾
  「這個變化值得處理嗎？」
         │ (符合)
         ▼
  WorkQueue
  放入 {namespace: "default", name: "my-collector"}
         │
         ▼
  Reconcile(ctx, Request{...})
  「好，我來讓現實符合期望」
```

---

## 7.9 為什麼 Reconcile() 拿到的是 name，不是物件？

你可能注意到 `Reconcile()` 的參數是 `ctrl.Request`（只有 name + namespace），而不是整個物件：

```go
func (r *OpenTelemetryCollectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var instance v1beta1.OpenTelemetryCollector
    if err := r.Get(ctx, req.NamespacedName, &instance); err != nil { ... }
```

原因有三：

1. **去重（Deduplication）**：如果同一個物件在短時間內被修改 5 次，WorkQueue 只會留一個 `{name, namespace}`，避免重複 Reconcile
2. **時效性**：從 WorkQueue 取出到實際執行之間可能有延遲。Reconcile 時再去讀最新狀態，確保使用的是最新的 spec，而不是過時的快照
3. **快取一致性**：`r.Get()` 從 Informer 的本地快取（Indexer）讀取，不會每次都打 API Server，速度極快

---

## 練習 7：閱讀理解與追蹤

### 問題 1

打開 `internal/controllers/targetallocator_controller.go` 第 209-218 行的 Predicate：

```go
predicate.NewPredicateFuncs(func(object client.Object) bool {
    collector := object.(*v1beta1.OpenTelemetryCollector)
    return collector.Spec.TargetAllocator.Enabled
}),
```

**問：** 如果一個 `OpenTelemetryCollector` 原本 `TargetAllocator.Enabled = true`，現在有人把它改成 `false`，這個 Predicate 會讓 Reconcile 觸發嗎？為什麼這可能是個問題？

<details>
<summary>參考答案</summary>

`predicate.NewPredicateFuncs` 會對 `OnAdd`、`OnUpdate`、`OnDelete` 都套用同一個函式。對於 `OnUpdate`，它只會收到**更新後的物件**（`false`），所以 Predicate 回傳 `false`，Reconcile 不會被觸發。

這是一個潛在問題：如果有 TargetAllocator CR 是因為 `OpenTelemetryCollector.Spec.TargetAllocator.Enabled = true` 而被建立的，現在 Enabled 改成 false，TargetAllocator Controller 不會被通知去清理它。

實際上，`OpenTelemetryCollector` 的 Reconciler 會在自己的 Reconcile 中處理 TargetAllocator CR 的生命週期，所以這個 Watch 的設計意圖是「只有 Enabled 時才可能需要更新 config」，而非處理刪除邏輯。
</details>

---

### 問題 2

`SetupCaches()` 在 `internal/controllers/opentelemetrycollector_controller.go` 第 315 行：

```go
func (r *OpenTelemetryCollectorReconciler) SetupCaches(cluster cluster.Cluster) error {
    for _, resource := range ownedResources {
        cluster.GetCache().IndexField(context.Background(), resource, resourceOwnerKey, ...)
    }
}
```

**問：** `IndexField` 建立了什麼？它和 Predicate 有什麼不同？

<details>
<summary>參考答案</summary>

`IndexField` 在 Informer 的本地快取（Indexer）上建立一個**索引**，讓你可以用 `resourceOwnerKey`（也就是 owner 的 name）快速查詢「哪些子資源屬於這個 CR」。

這用在 `findOtelOwnedObjects()` 中：不需要列出所有 Deployment 再手動過濾，而是直接用索引查出屬於特定 `OpenTelemetryCollector` 的子資源。

**與 Predicate 的差異：**
- Predicate：決定「這個事件要不要觸發 Reconcile」
- IndexField：決定「在 Reconcile 內查詢資料時，怎麼快速找到相關資源」

兩者在不同的時間點發揮作用：Predicate 在事件進入 WorkQueue 前，IndexField 在 Reconcile 執行中。
</details>

---

### 問題 3（動手）

用 `curl` 親眼看一次 HTTP Watch（需要有 `kubectl` 可用的環境）：

```bash
# 取得 API Server 的位置
kubectl cluster-info

# 用 kubectl proxy 在本地開一個代理（不用處理 TLS）
kubectl proxy --port=8001 &

# 發起 Watch 請求，然後用 kubectl 在另一個終端建立或修改資源
curl -N "http://localhost:8001/apis/opentelemetry.io/v1beta1/opentelemetrycollectors?watch=true"
```

**觀察：** 每當你在另一個終端執行 `kubectl apply` 或 `kubectl delete`，curl 視窗會出現什麼？注意 `type` 欄位與 `resourceVersion` 的變化。
