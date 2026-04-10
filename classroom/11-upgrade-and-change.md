# 第 11 章：升級與異動機制

> **本章目標：** 理解當 CR 的 Spec 改變、CRD schema 升級、或 Operator 本身版本更新時，系統各層面如何反應

---

## 11.1 三種「變化」的場景

Operator 上線後，最常遇到的變化有三類：

```
變化類型一：CR Spec 異動（使用者改了 YAML）
  e.g. 把 replicas: 2 改成 replicas: 5
  → Reconcile 被觸發，Operator 同步 Deployment

變化類型二：Operator 版本升級（新功能 / bug fix）
  e.g. 新版 Operator image 部署到 cluster
  → 新版 Controller 重新 Reconcile 所有現有 CR

變化類型三：CRD Schema 升級（API 欄位新增 / 改名）
  e.g. FooApp v1alpha1 加了新欄位 port
  → 需要注意向後相容性、欄位預設值
```

本章依序演練這三類，並對照 OTel Operator 的處理方式。

---

## 11.1 場景一：CR Spec 異動

### 修改 replicas

```bash
# 目前狀態
kubectl get fooapp hello-foo -o yaml
# spec:
#   image: nginx:alpine
#   replicas: 2

# 方法一：直接 patch
kubectl patch fooapp hello-foo \
  --type=merge \
  -p '{"spec":{"replicas":5}}'

# 方法二：edit（開啟 editor）
kubectl edit fooapp hello-foo
```

### 觀察 Reconcile 反應

```bash
# 查看 log（會看到同步的訊息）
kubectl logs -n foo-operator-system \
  -l control-plane=controller-manager \
  --follow

# 驗證 Deployment 已同步
kubectl get deployment hello-foo
# READY 會從 2/2 → 5/5
```

### 追蹤事件鏈

```
使用者執行 kubectl patch fooapp
         ↓
API server 更新 FooApp 物件（resourceVersion 遞增）
         ↓
controller-runtime Watch 收到 MODIFIED 事件
         ↓
FooApp 的 NamespacedName 放入 reconcile queue
         ↓
Reconcile() 執行：
  r.Get → 讀到新的 Spec（replicas=5）
  buildDeployment → 建構期望 Deployment
  r.Update → 更新現有 Deployment 的 replicas
         ↓
Deployment controller（k8s 內建）偵測到 replicas 變化
  → 新增 3 個 Pod
         ↓
Deployment.status.readyReplicas 更新
  → 觸發我們的 Reconcile（因為 Owns(Deployment)）
         ↓
Reconcile() 執行：
  Status().Update → 更新 FooApp.Status.ReadyReplicas = 5
```

---

## 11.2 場景二：Operator 版本升級

### 修改程式碼，發布新版

假設我們修了一個 bug：之前 `buildDeployment` 沒有設定 `resources.limits`，新版加上了。

```go
// 新版的 buildDeployment（新增 Resources 設定）
containers: []corev1.Container{
    {
        Name:  "app",
        Image: fooApp.Spec.Image,
        Resources: corev1.ResourceRequirements{
            Limits: corev1.ResourceList{
                corev1.ResourceMemory: resource.MustParse("128Mi"),
                corev1.ResourceCPU:    resource.MustParse("100m"),
            },
        },
    },
},
```

### 部署新版

```bash
# 重新 build
IMG=foo-operator:v2 make docker-build

# 載入到 k3d
k3d image import foo-operator:v2 --cluster foo-dev

# 更新 Operator Deployment（改 image tag）
kubectl set image deployment/foo-operator-controller-manager \
  manager=foo-operator:v2 \
  -n foo-operator-system

# 觀察 rollout
kubectl rollout status deployment/foo-operator-controller-manager \
  -n foo-operator-system
```

### 新版 Operator 啟動後發生什麼？

```
新版 Operator Pod 啟動
         ↓
controller-runtime Manager 初始化
         ↓
List 所有現有 FooApp CR（Informer 初始化）
         ↓
對每個 FooApp 觸發 Reconcile
（這是 Operator 重啟後的「全量同步」行為）
         ↓
新的 buildDeployment 包含 resources.limits
  → r.Update 把 resources.limits 加到現有 Deployment
         ↓
所有現有 FooApp 對應的 Deployment 都補上了 limits
```

**這就是 Operator 升級的核心優勢**：不需要使用者重新 apply CR，新版 Operator 自動對所有 CR 重新 Reconcile，存量資源一起升級。

### OTel Operator 的版本升級機制

OTel Operator 有更複雜的升級路徑：

```
pkg/collector/upgrade/
├── upgrade.go          ← 版本比較入口
├── v0_19_0.go          ← 特定版本的遷移邏輯
├── v0_31_0.go
└── ...
```

在 Reconcile 的第一步就會檢查：

```go
// internal/controllers/opentelemetrycollector_controller.go（第 265-272 行）
if r.upgrade.NeedsUpgrade(instance) {
    err = r.upgrade.Upgrade(ctx, instance)
    return ctrl.Result{Requeue: true, RequeueAfter: 1 * time.Second}, nil
}
```

`NeedsUpgrade()` 比較 CR 上記錄的 `version` annotation 和 Operator 目前版本，若版本落後則執行對應的 migration function，這讓每個版本的遷移邏輯可以獨立、可測試。

---

## 11.3 場景三：CRD Schema 升級

### 新增欄位（向後相容）

假設新版 FooApp 要加入 `port` 欄位：

```go
// api/v1alpha1/fooapp_types.go（修改後）
type FooAppSpec struct {
    Image    string `json:"image"`
    Replicas *int32 `json:"replicas,omitempty"`

    // Port 是 container 對外開放的 port，預設 8080
    // +kubebuilder:default=8080
    // +optional
    Port int32 `json:"port,omitempty"`
}
```

因為 `port` 是 optional 且有 default value，**舊的 CR 不需要修改**，仍然合法。

```bash
# 重新生成 CRD YAML
make generate && make manifests

# 更新 cluster 上的 CRD
make install

# 驗證：舊 CR 仍然存在，且新欄位用預設值
kubectl get fooapp hello-foo -o yaml
# spec.port 會出現 8080（default 值）
```

### 移除或重命名欄位（破壞性變更）

如果要把 `replicas` 改名為 `replicaCount`，必須注意：

```
破壞性變更的風險：
  - 舊版 CR（有 replicas 欄位）apply 到新版 CRD 時，replicas 會被忽略
  - 使用者的 GitOps YAML 需要同步更新

安全的做法：
  1. 新舊欄位並存一段時間（deprecation period）
  2. 在新欄位加 annotation 說明，在舊欄位加 deprecated 說明
  3. Operator 在 Reconcile 時，優先讀新欄位，若空則 fallback 到舊欄位
  4. 下下個版本才移除舊欄位
```

OTel Operator 的做法範例（第 5 章提到的 `pkg/collector/upgrade/`）：

```go
// pkg/collector/upgrade/v0_XX_0.go（示意）
func upgrade0_XX_0(ctx context.Context, client client.Client, otelcol *v1beta1.OpenTelemetryCollector) error {
    // 把舊欄位的值遷移到新欄位
    if otelcol.Spec.OldField != "" && otelcol.Spec.NewField == "" {
        otelcol.Spec.NewField = otelcol.Spec.OldField
        otelcol.Spec.OldField = ""
    }
    return client.Update(ctx, otelcol)
}
```

---

## 11.4 樂觀鎖與衝突處理

當 CR 被快速連續修改（例如 CI 自動化或多個 controller 同時寫），Reconcile 可能遇到衝突：

```
衝突場景：
  1. Reconcile 讀取 CR（resourceVersion = 42）
  2. 使用者在 Reconcile 執行中改了 CR（resourceVersion → 43）
  3. Reconcile 想 Update Status（帶著 resourceVersion=42）
  4. API server 拒絕：409 Conflict
```

controller-runtime 的標準處理方式：

```go
// 在 Reconcile 裡的 Status Update
if err := r.Status().Update(ctx, &fooApp); err != nil {
    if errors.IsConflict(err) {
        // 版本衝突，直接 requeue，讓 controller 重新讀最新版本
        return ctrl.Result{Requeue: true}, nil
    }
    return ctrl.Result{}, err
}
```

OTel Operator 用 `client-go` 的 retry 機制處理（第 8 章提到的 `k8s.io/client-go/util/retry`）：

```go
// internal/controllers/common.go（示意）
retry.RetryOnConflict(retry.DefaultRetry, func() error {
    // 重新 Get 最新版本，再 Update
    return r.Status().Update(ctx, instance)
})
```

---

## 11.5 異動觀察工具

### 看 CR 事件記錄

```bash
# kubectl describe 會顯示 Events
kubectl describe fooapp hello-foo

# 輸出的 Events 區段：
# Events:
#   Type    Reason   Age  From               Message
#   ----    ------   ---  ----               -------
#   Normal  Created  5m   fooapp-controller  建立 Deployment hello-foo
```

要讓 Operator 發出 Events，需要在 Controller 裡使用 `record.EventRecorder`：

```go
type FooAppReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder   // 新增
}

// 在 Reconcile 裡發出 Event
r.Recorder.Event(&fooApp, corev1.EventTypeNormal, "Created", "建立 Deployment "+desired.Name)
```

### 即時監看所有變化

```bash
# 監看 FooApp 的所有欄位變化
kubectl get fooapp --watch

# 監看所有相關資源
kubectl get fooapp,deployment,pod -l app=hello-foo --watch
```

---

## 11.6 完整的 FooApp 生命週期

```
建立（Create）
  kubectl apply fooapp.yaml
         ↓
  Reconcile: Deployment not found → Create
  Reconcile: Status.ReadyReplicas = 0（Pod 還在 Pending）
  ...幾秒後...
  Reconcile: Status.ReadyReplicas = 2（Pod Running）

更新（Update）
  kubectl patch fooapp ... replicas=5
         ↓
  Reconcile: Update Deployment.Spec.Replicas = 5
  Reconcile: Status.ReadyReplicas = 2（新 Pod 還在啟動）
  ...幾秒後...
  Reconcile: Status.ReadyReplicas = 5

刪除（Delete）
  kubectl delete fooapp hello-foo
         ↓
  k8s GC 偵測到 FooApp 被刪除
  → Deployment 有 OwnerReference 指向 FooApp
  → GC 自動刪除 Deployment
  → Deployment 刪除後，Pod 也被刪除
  （Reconcile 在這裡會收到 Not Found，直接 return nil）
```

---

## 練習 1：觸發 409 衝突

**問：** 以下哪種操作最容易觸發 409 Conflict？為什麼？

A. 使用者慢慢一個一個 apply CR
B. CI 系統快速連續 patch 同一個 CR 多次
C. 手動 kubectl delete 一個 Pod

<details>
<summary>參考答案</summary>

B。每次 patch 都會遞增 resourceVersion。如果 Reconcile 在兩次 patch 之間執行，它讀到的 resourceVersion 比最新值舊，Update 時就會遇到 409。

C 不會導致 CR 的衝突，因為刪 Pod 只觸發 Reconcile 讀 CR，沒有去 Update CR 本身。

</details>

## 練習 2：觀察升級行為

1. 修改 `buildDeployment`，加入一個 label `version: v2` 到 PodTemplate
2. build 新 image、import 到 k3d、rollout restart Operator
3. 觀察：現有 `hello-foo` 的 Pod 有沒有自動被重建並帶上新 label？

**問：** Pod 被重建是因為 Operator 主動刪掉再建，還是因為 Deployment controller 偵測到 PodTemplate 變化？

<details>
<summary>參考答案</summary>

是 Deployment controller（k8s 內建）做的。

Operator 只更新了 Deployment 的 `spec.template.labels`（PodTemplate 變化）。Deployment controller 偵測到 PodTemplate 的 hash 不同，按照 `RollingUpdate` 策略，逐步替換 Pod（先建新 Pod，確認健康後刪舊 Pod）。

Operator 不需要知道 Pod 的存在，也不需要主動管理 Pod 的生命週期。這是「層層委派」的設計：Operator 管 Deployment，Deployment 管 ReplicaSet，ReplicaSet 管 Pod。

</details>

---

[← 第 10 章：部署到 k3d](./10-deploy-to-k3d.md) | [← 回目錄](./00-overview.md)
