# 第 9 章：從零建立一個簡單的 Operator

> **本章目標：** 用 kubebuilder 從頭腳手架一個最小可運行的 Operator，親手經歷 CRD 定義 → Controller 撰寫 → 本地測試的完整流程

---

## 9.1 我們要做什麼

前八章是「讀懂」OTel Operator 的程式碼。第 9-11 章換個角度：**自己從零建一個 Operator**，讓概念變成肌肉記憶。

我們會建一個叫做 **`FooApp` Operator**，功能很簡單：

```
使用者建立一個 FooApp CR，指定：
  - image：要跑什麼 container image
  - replicas：幾個副本

Operator 負責：
  - 建立對應的 Deployment
  - 當 FooApp 被刪除，Deployment 自動跟著消失
  - 當 replicas 改變，Deployment 立刻同步
  - 把目前 readyReplicas 回寫到 FooApp.Status
```

這個範圍夠小，但涵蓋了所有核心機制：CRD、Controller、OwnerReference、Status 更新。

---

## 9.2 環境準備

```bash
# 確認版本
go version           # >= 1.22（本機：1.25.1）
kubebuilder version  # >= 4.x（本機：4.10.1）
kubectl version      # 能連到任何 cluster 即可（本機：v1.35.0）

# 建立新專案目錄
mkdir ~/foo-operator && cd ~/foo-operator
```

---

## 9.3 腳手架（Scaffold）

kubebuilder 會替你生成所有樣板程式碼，讓你專注在業務邏輯。

```bash
# 步驟 1：初始化 Go module 和 operator 骨架
kubebuilder init \
  --domain example.com \
  --repo github.com/yourname/foo-operator

# 步驟 2：建立 API（CRD + Controller）
kubebuilder create api \
  --group apps \
  --version v1alpha1 \
  --kind FooApp \
  --resource \
  --controller
```

> **kubebuilder v4 注意事項：** v4 預設使用 `--plugins go/v4`，與 v3 的主要差異是 `main.go` 改放在 `cmd/main.go`，controller 放在 `internal/controller/`（v3 是 `controllers/`）。遇到互動式提示「Create Resource? [y/n]」和「Create Controller? [y/n]」一律輸入 `y`。

執行後，專案結構長這樣：

```
foo-operator/
├── cmd/
│   └── main.go                      ← Manager 進入點
├── api/
│   └── v1alpha1/
│       ├── fooapp_types.go          ← 你要填寫 Spec/Status 的地方  ★
│       ├── groupversion_info.go     ← GroupVersion 常數，不需要手動改
│       └── zz_generated.deepcopy.go ← 自動生成，不要手動改
├── internal/
│   └── controller/
│       ├── fooapp_controller.go     ← Reconcile 邏輯放這裡  ★
│       ├── fooapp_controller_test.go← 單元測試（envtest）
│       └── suite_test.go            ← 測試 suite 設定
├── config/
│   ├── crd/                         ← CRD kustomize 設定
│   ├── default/                     ← 預設部署組合（含 metrics、cert 相關 patch）
│   ├── manager/
│   │   └── manager.yaml             ← Operator 本身的 Deployment YAML
│   ├── rbac/                        ← ServiceAccount、Role、RoleBinding
│   │   ├── role.yaml                ← 從 marker 生成的 RBAC 規則
│   │   └── ...
│   ├── samples/
│   │   └── apps_v1alpha1_fooapp.yaml← 範例 CR YAML
│   ├── network-policy/              ← 允許 metrics 流量的 NetworkPolicy
│   └── prometheus/                  ← ServiceMonitor（接 Prometheus Operator）
├── test/
│   ├── e2e/                         ← E2E 測試骨架
│   └── utils/
├── hack/
│   └── boilerplate.go.txt           ← 每個新檔案的 license header 模板
├── bin/
│   └── controller-gen               ← make generate 用的工具（本地安裝）
├── Makefile
├── Dockerfile
├── go.mod
└── go.sum
```

**標 ★ 的是本章主要修改的兩個檔案**，其餘都是腳手架生成的樣板，大部分情況不需要動。

> `config/default/` 比 v3 多了 metrics、cert 相關的 patch，這是 v4 預設整合了 controller-runtime 的 metrics server（含 TLS）。如果本機開發不需要這些功能，之後 `make deploy` 遇到問題時可以在 `config/default/kustomization.yaml` 把相關 patch 註解掉。

---

## 9.4 程式碼生成：`make generate` 和 `make manifests`

腳手架建好後，第一件事是執行這兩個命令，理解它們的作用有助於之後修改 struct 時知道該重跑哪個。

```bash
make generate   # 生成 DeepCopy 方法
make manifests  # 生成 CRD / RBAC YAML
```

### `make generate`

```bash
controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."
```

**生成 `DeepCopy` 方法**，輸出到 `api/v1alpha1/zz_generated.deepcopy.go`。

Kubernetes 的 API machinery 要求每個物件都能被深度複製（避免多個 goroutine 共享同一個指標導致資料競爭）。controller-gen 掃描有 `// +kubebuilder:object:root=true` marker 的 struct，自動生成：

```go
// zz_generated.deepcopy.go（自動生成，不要手動改）
func (in *FooApp) DeepCopyObject() runtime.Object { ... }
func (in *FooAppSpec) DeepCopyInto(out *FooAppSpec) { ... }
func (in *FooAppList) DeepCopyObject() runtime.Object { ... }
```

**何時重跑：** 每次修改 `fooapp_types.go` 的 struct 定義後。

---

### `make manifests`

```bash
controller-gen rbac:roleName=manager-role crd webhook paths="./..." \
  output:crd:artifacts:config=config/crd/bases
```

**從程式碼中的 marker 生成三種 YAML：**

| 生成什麼 | 來源 | 輸出位置 |
|---|---|---|
| CRD YAML | `fooapp_types.go` 的 struct + kubebuilder marker | `config/crd/bases/` |
| RBAC YAML | `fooapp_controller.go` 上方的 `// +kubebuilder:rbac:...` | `config/rbac/role.yaml` |
| Webhook YAML | `// +kubebuilder:webhook:...` marker | `config/webhook/` |

例如你在 Spec 加了驗證 marker：

```go
// +kubebuilder:validation:Minimum=0
Replicas *int32 `json:"replicas,omitempty"`
```

`make manifests` 就會把這條規則寫進 CRD YAML 的 `openAPIV3Schema`，讓 API server 在 `kubectl apply` 時就擋掉不合法的值，**不需要等 Reconcile 執行才發現錯誤**。

**何時重跑：** 修改 struct 上的 kubebuilder marker，或修改 controller 的 RBAC marker 後。

---

### 兩者的執行順序

```
你改了 fooapp_types.go
        ↓
make generate   → 更新 zz_generated.deepcopy.go（Go 編譯需要）
        ↓
make manifests  → 更新 config/crd/bases/*.yaml（部署到 cluster 需要）
        ↓
make install    → kubectl apply config/crd/bases/*.yaml
                  → cluster 認識新的 CRD 結構
```

**對比 OTel Operator：** 這就是 OTel Operator 的 `make generate` 和 `make manifests` 在做的同一件事。OTel Operator 的 `apis/v1beta1/zz_generated.deepcopy.go` 也是這樣生成的，**永遠不要手動編輯 `zz_generated.*` 開頭的檔案**。

---

## 9.5 定義 CRD：FooApp 的 Spec 和 Status

打開 `api/v1alpha1/fooapp_types.go`，把預設的空 Spec 改成：

```go
// api/v1alpha1/fooapp_types.go

// FooAppSpec 描述使用者期望的狀態
type FooAppSpec struct {
    // Image 是要部署的 container image
    // +kubebuilder:validation:Required
    Image string `json:"image"`

    // Replicas 是副本數，預設為 1
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:default=1
    // +optional
    Replicas *int32 `json:"replicas,omitempty"`
}

// FooAppStatus 描述 Operator 觀察到的實際狀態
type FooAppStatus struct {
    // ReadyReplicas 是目前健康的副本數
    ReadyReplicas int32 `json:"readyReplicas,omitempty"`

    // Conditions 記錄各種狀態條件（標準 Kubernetes 格式）
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

**和 OTel Operator 的對比：**

| 概念 | OTel Operator | 我們的 FooApp |
|---|---|---|
| Spec | `apis/v1beta1/opentelemetrycollector_types.go` | `api/v1alpha1/fooapp_types.go` |
| Status | `OpenTelemetryCollectorStatus` | `FooAppStatus` |
| kubebuilder markers | `+kubebuilder:validation:*` | 同上 |

生成 CRD YAML：

```bash
make generate   # 生成 DeepCopy 方法
make manifests  # 生成 CRD、RBAC YAML
```

---

## 9.6 實作 Reconcile 邏輯

打開 `internal/controller/fooapp_controller.go`，填入業務邏輯：

```go
// internal/controller/fooapp_controller.go

package controller

import (
    "context"
    appsv1 "k8s.io/api/apps/v1"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

    appsv1alpha1 "github.com/yourname/foo-operator/api/v1alpha1"
)

type FooAppReconciler struct {
    client.Client
    Scheme *runtime.Scheme
}

func (r *FooAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := ctrl.LoggerFrom(ctx)

    // 步驟 1：讀取 CR（期望狀態）
    var fooApp appsv1alpha1.FooApp
    if err := r.Get(ctx, req.NamespacedName, &fooApp); err != nil {
        // 已被刪除，OwnerReference 會幫我們清理 Deployment
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 步驟 2：建構期望的 Deployment（純函式，不呼叫 API）
    desired := r.buildDeployment(&fooApp)

    // 步驟 3：CreateOrUpdate（內建衝突重試，解決樂觀鎖問題）
    existing := &appsv1.Deployment{
        ObjectMeta: metav1.ObjectMeta{
            Name:      fooApp.Name,
            Namespace: fooApp.Namespace,
        },
    }
    op, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
        // 設定 OwnerReference（Create 和 Update 都確保）
        if err := ctrl.SetControllerReference(&fooApp, existing, r.Scheme); err != nil {
            return err
        }
        // 同步 Spec（只更新 Operator 關心的欄位）
        existing.Spec.Replicas = desired.Spec.Replicas
        existing.Spec.Selector = desired.Spec.Selector
        existing.Spec.Template = desired.Spec.Template
        return nil
    })
    if err != nil {
        return ctrl.Result{}, err
    }
    log.Info("Deployment 已同步", "operation", op, "name", existing.Name)

    // 步驟 4：回寫 Status
    fooApp.Status.ReadyReplicas = existing.Status.ReadyReplicas
    if err := r.Status().Update(ctx, &fooApp); err != nil {
        log.Error(err, "無法更新 Status")
    }

    return ctrl.Result{}, nil
}

// buildDeployment 根據 FooApp Spec 生成 Deployment 物件（純函式，不呼叫 API）
func (r *FooAppReconciler) buildDeployment(fooApp *appsv1alpha1.FooApp) *appsv1.Deployment {
    replicas := int32(1)
    if fooApp.Spec.Replicas != nil {
        replicas = *fooApp.Spec.Replicas
    }
    return &appsv1.Deployment{
        ObjectMeta: metav1.ObjectMeta{
            Name:      fooApp.Name,
            Namespace: fooApp.Namespace,
        },
        Spec: appsv1.DeploymentSpec{
            Replicas: &replicas,
            Selector: &metav1.LabelSelector{
                MatchLabels: map[string]string{"app": fooApp.Name},
            },
            Template: corev1.PodTemplateSpec{
                ObjectMeta: metav1.ObjectMeta{
                    Labels: map[string]string{"app": fooApp.Name},
                },
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{
                        {
                            Name:  "app",
                            Image: fooApp.Spec.Image,
                        },
                    },
                },
            },
        },
    }
}

// SetupWithManager 告訴 controller-runtime 監聽什麼資源
func (r *FooAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&appsv1alpha1.FooApp{}).
        Owns(&appsv1.Deployment{}).  // 子資源有變動也觸發 Reconcile
        Complete(r)
}
```

**關鍵設計決策解說：**

```
為什麼用 controllerutil.CreateOrUpdate，而不是手動 Get + Create/Update？

手動寫法的問題：
  r.Get → 拿到 resourceVersion=5
  （k8s 內部修改 Deployment，resourceVersion → 6）
  r.Update（帶著 resourceVersion=5）→ 409 Conflict！

controllerutil.CreateOrUpdate 的內部流程：
  1. r.Get(existing)
     ├── Not Found → mutate() → r.Create
     └── Found     → mutate() → r.Update
  若 Update 遇到 409 → 自動重新 Get 最新版本，再 mutate，再 Update

這就是第 11 章講的「樂觀鎖衝突處理」的正確實作方式。
```

```
為什麼 buildDeployment 是純函式（不呼叫 k8s API）？

這是 OTel Operator 第 3 章（manifests builder）的設計原則：
  - 純函式可以單元測試（不需要 cluster）
  - 邏輯清晰：從 Spec 到物件，無副作用
  - CreateOrUpdate 的 mutate 函式只需要「把 desired 的欄位賦值給 existing」

對比：OTel Operator 的 internal/manifests/collector/deployment.go
也是純函式設計，Build() 不呼叫任何 API。
```

---

## 9.7 設定 RBAC Marker

Controller 需要操作 Deployment，在 Reconcile 函式上方加上 marker：

```go
// internal/controller/fooapp_controller.go

// +kubebuilder:rbac:groups=apps.example.com,resources=fooapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.example.com,resources=fooapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

func (r *FooAppReconciler) Reconcile(...) { ... }
```

然後重新生成 RBAC：

```bash
make manifests
```

---

## 9.8 本地執行測試

```bash
# 安裝 CRD 到當前 cluster（只裝定義，不跑 Operator）
make install

# 驗證 CRD 已安裝
kubectl get crd fooapps.apps.example.com

# 在本地跑 Operator（terminal 1）
make run

# 建立一個 FooApp CR（terminal 2）
kubectl apply -f - <<EOF
apiVersion: apps.example.com/v1alpha1
kind: FooApp
metadata:
  name: my-fooapp
  namespace: default
spec:
  image: nginx:alpine
  replicas: 2
EOF

# 觀察 Operator terminal 的 log
# 應該看到：「建立 Deployment」的訊息

# 驗證 Deployment 已建立
kubectl get deployment my-fooapp
kubectl get fooapp my-fooapp -o yaml  # 查看 Status
```

---

## 9.9 OwnerReference 的作用

```bash
# 查看 Deployment 的 ownerReferences
kubectl get deployment my-fooapp -o jsonpath='{.metadata.ownerReferences}' | jq .

# 輸出類似：
# [{"apiVersion":"apps.example.com/v1alpha1","kind":"FooApp",
#   "name":"my-fooapp","uid":"abc-123","controller":true}]

# 現在刪除 FooApp
kubectl delete fooapp my-fooapp

# Deployment 會跟著被 GC 清理（因為 ownerReference）
kubectl get deployment my-fooapp  # 幾秒後消失
```

這就是第 5 章講的 **Garbage Collection 機制**：只要設好 OwnerReference，子資源會自動清理，不需要手動在 Finalizer 裡處理。

---

## 9.10 與 OTel Operator 的對比

| 功能 | FooApp（本章）| OTel Operator |
|---|---|---|
| CRD 定義 | `api/v1alpha1/fooapp_types.go` | `apis/v1beta1/opentelemetrycollector_types.go` |
| Reconcile | `internal/controller/fooapp_controller.go` | `internal/controllers/opentelemetrycollector_controller.go` |
| 資源生成 | `buildDeployment()`（同一個檔案）| `internal/manifests/collector/`（獨立套件） |
| 子資源清理 | OwnerReference（GC 自動處理）| OwnerReference + Finalizer（處理 cluster-scoped 資源）|
| 升級邏輯 | 無 | `pkg/collector/upgrade/`（版本遷移）|

OTel Operator 比較複雜的原因：有四種 deployment mode（Deployment/DaemonSet/StatefulSet/Sidecar）、需要處理 cluster-scoped 資源、有複雜的升級路徑。我們的 FooApp 把這些都去掉，只留下骨架。

---

## 練習 1：閱讀理解

現在 `Reconcile()` 裡的 Update 邏輯只同步了 `replicas` 和 `image`，如果使用者手動修改了 Deployment 的 label，Operator 不會還原。

**問：** 如何讓 Operator 確保 Deployment 完全符合期望狀態（包含 labels）？

<details>
<summary>參考答案</summary>

有兩種做法：

1. **完整覆寫**：`existing.Spec = desired.Spec`（簡單但可能丟失某些 k8s 自動加入的欄位）

2. **三向合併**（OTel Operator 的做法）：用 `internal/manifests/mutate.go` 的 `MutateFuncFor()`，只更新 Operator 關心的欄位，保留其他欄位不動。

OTel Operator 選擇做法 2，因為某些欄位（例如 `resourceVersion`、`clusterIP`）必須從 existing 物件取得，不能直接覆蓋。

</details>

## 練習 2：動手題

在 `FooAppStatus` 加入一個 `Phase` 欄位（`string`），值為 `"Running"` 或 `"Pending"`，根據 `ReadyReplicas == Spec.Replicas` 來決定。

修改步驟：
1. 在 `fooapp_types.go` 加欄位
2. 在 `Reconcile()` 填入邏輯
3. `make generate && make manifests`
4. `make run` 後驗證 `kubectl get fooapp -o yaml`

---

[← 第 8 章：重要 Library](./08-key-libraries.md) | [第 10 章：部署到 k3d →](./10-deploy-to-k3d.md)
