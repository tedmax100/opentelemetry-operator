# 第 10 章：部署 Operator 到 k3d 環境

> **本章目標：** 把第 9 章建立的 FooApp Operator 打包成 container image，部署到本機的 k3d cluster，實際跑起來

---

## 10.1 為什麼需要 k3d？

第 9 章用 `make run` 在本機跑 Operator，這種方式有限制：

```
make run 的問題：
  - Webhook 無法運作（需要 in-cluster TLS）
  - Operator 和 k8s API server 之間靠本機網路，不是 cluster 內通信
  - 無法測試 RBAC 設定是否正確
  - 無法測試 image pull policy、resource limits 等 Pod 級別設定
```

**k3d** 讓你在本機用 Docker 跑一個完整的 Kubernetes cluster（底層是 k3s），適合本機開發和 CI 測試。

```
k3d 架構：

  你的 macOS / Linux
  ┌────────────────────────────────────────────────┐
  │  Docker                                        │
  │  ┌──────────────────────────────────────────┐  │
  │  │  k3d cluster（名稱：foo-dev）             │  │
  │  │                                          │  │
  │  │  ┌─────────────┐  ┌─────────────────┐   │  │
  │  │  │ control-plane│  │   agent node    │   │  │
  │  │  │ (k3s server) │  │  (k3s agent)    │   │  │
  │  │  └─────────────┘  └─────────────────┘   │  │
  │  └──────────────────────────────────────────┘  │
  │                                                │
  │  kubectl ──→ localhost:6443 ──→ API server     │
  └────────────────────────────────────────────────┘
```

---

## 10.2 建立 k3d Cluster

```bash
# 建立 cluster，對外開放 API server 在 port 6443
k3d cluster create foo-dev \
  --agents 1 \
  --port "8080:80@loadbalancer"

# 驗證
kubectl cluster-info
kubectl get nodes
# 應看到 control-plane + agent 共 2 個節點
```

---

## 10.3 打包 Operator Image

kubebuilder 生成的 Makefile 已包含 `docker-build` 目標。

```bash
# 在 foo-operator 目錄內執行

# 方法一：用 Dockerfile 打包（需要 Docker）
IMG=foo-operator:dev make docker-build

# 方法二：不推送到 registry，直接載入到 k3d cluster
k3d image import foo-operator:dev --cluster foo-dev
```

`k3d image import` 把本機 Docker image 直接載入 k3d 的所有節點，**不需要 image registry**，是本機開發最快的方法。

---

## 10.4 部署 Operator

```bash
# 安裝 CRD
make install

# 驗證
kubectl get crd fooapps.apps.example.com

# 部署 Operator（Deployment + RBAC + ...）
IMG=foo-operator:dev make deploy

# 查看 Operator Pod 是否正常運行
kubectl get pods -n foo-operator-system
# 應看到 foo-operator-controller-manager-XXXX Running
```

**`make deploy` 做了什麼？**

```
make deploy 實際上執行：
  kustomize build config/default | kubectl apply -f -

config/default/ 包含：
  ├── config/crd/         ← CRD（也可單獨用 make install 裝）
  ├── config/rbac/        ← ServiceAccount、ClusterRole、ClusterRoleBinding
  ├── config/manager/     ← Operator 本身的 Deployment
  └── kustomization.yaml  ← 把以上組合起來
```

---

## 10.5 觀察 Operator 啟動流程

```bash
# 查看 Operator 的 log
kubectl logs -n foo-operator-system \
  -l control-plane=controller-manager \
  --follow
```

正常啟動時，log 會包含：

```
INFO    Starting manager
INFO    Starting EventSource  {"controller": "fooapp", "source": "kind source: *v1alpha1.FooApp"}
INFO    Starting EventSource  {"controller": "fooapp", "source": "kind source: *v1.Deployment"}
INFO    Starting Controller   {"controller": "fooapp"}
INFO    Starting workers      {"controller": "fooapp", "worker count": 1}
```

對應到程式碼 `SetupWithManager()` 裡的 `.For()` 和 `.Owns()`：每一行 EventSource 對應一個監聽來源。

---

## 10.6 建立 CR，觸發 Reconcile

```bash
# 建立 FooApp CR
kubectl apply -f - <<EOF
apiVersion: apps.example.com/v1alpha1
kind: FooApp
metadata:
  name: hello-foo
  namespace: default
spec:
  image: nginx:alpine
  replicas: 2
EOF

# 立刻觀察 log（terminal 2）
kubectl logs -n foo-operator-system \
  -l control-plane=controller-manager \
  --follow

# 查看建立的資源
kubectl get fooapp hello-foo
kubectl get deployment hello-foo
kubectl get pods -l app=hello-foo
```

預期看到的 log：

```
INFO    建立 Deployment   {"controller": "fooapp", "name": "hello-foo", "namespace": "default"}
```

預期 Status 已更新：

```bash
kubectl get fooapp hello-foo -o yaml
# ...
# status:
#   readyReplicas: 2
```

---

## 10.7 完整的 k3d 開發迴圈

實際開發時會反覆修改程式碼 → 重新部署，標準流程如下：

```
┌─────────────────────────────────────────────────────┐
│                                                     │
│  1. 修改程式碼（fooapp_controller.go 等）            │
│              ↓                                      │
│  2. 重新 build image                                │
│     IMG=foo-operator:dev make docker-build          │
│              ↓                                      │
│  3. 重新載入到 k3d                                   │
│     k3d image import foo-operator:dev --cluster foo-dev │
│              ↓                                      │
│  4. 重啟 Operator Pod（讓它拉新 image）               │
│     kubectl rollout restart deployment \            │
│       -n foo-operator-system \                      │
│       foo-operator-controller-manager               │
│              ↓                                      │
│  5. 觀察 log，測試 CR                                │
│     kubectl logs ... --follow                       │
│              ↓                                      │
│  └── 回到步驟 1                                      │
│                                                     │
└─────────────────────────────────────────────────────┘
```

**提示：** 為了讓 k3d 每次都拉新 image（即使 tag 沒變），可以在 `config/manager/manager.yaml` 把 `imagePullPolicy` 改為 `Never`（因為 image 是直接 import 進去，不需要從 registry pull）：

```yaml
# config/manager/manager.yaml
containers:
- image: foo-operator:dev
  name: manager
  imagePullPolicy: Never   # ← 加這行
```

---

## 10.8 RBAC 驗證

`make run` 時，Operator 用你的 kubeconfig 身份（通常是 admin），不受 RBAC 限制。部署到 cluster 後，Operator 用 ServiceAccount，RBAC 設定錯誤會在這一步才浮現。

常見的 RBAC 錯誤：

```bash
# Operator log 中看到類似：
# ERROR  Reconciler error  {"error": "deployments.apps is forbidden:
#   User \"system:serviceaccount:foo-operator-system:foo-operator-controller-manager\"
#   cannot create resource \"deployments\" in API group \"apps\" in namespace \"default\""}
```

修復方式：在 Controller 的 kubebuilder RBAC marker 加上缺少的權限，然後：

```bash
make manifests   # 重新生成 RBAC YAML
make deploy      # 重新套用
```

---

## 10.9 清理環境

```bash
# 移除 Operator 和 RBAC
make undeploy

# 移除 CRD（會連帶刪除所有 FooApp CR）
make uninstall

# 刪除 k3d cluster（完全清空）
k3d cluster delete foo-dev
```

---

## 練習 1：觀察刪除行為

```bash
# 先確認 Deployment 存在
kubectl get deployment hello-foo

# 手動刪掉 Deployment
kubectl delete deployment hello-foo

# 觀察：Operator 有沒有重新建立它？
# （觀察 log 和 kubectl get deployment）
```

**問：** 為什麼 Operator 會重新建立被刪掉的 Deployment？觸發 Reconcile 的事件是什麼？

<details>
<summary>參考答案</summary>

因為 `SetupWithManager()` 裡有 `.Owns(&appsv1.Deployment{})`，代表 Controller 也監聽它建立的 Deployment。

當 Deployment 被刪除，controller-runtime 收到 DELETED 事件，從 OwnerReference 找到對應的 FooApp，然後把 FooApp 的 reconcile 請求放入 queue。

Reconcile 執行時，`r.Get(ctx, req.NamespacedName, &existing)` 回傳 `IsNotFound`，進入 Create 分支，重新建立 Deployment。

這就是 Operator 「自愈（Self-healing）」的機制。
</details>

## 練習 2：動手部署

按照本章步驟，把第 9 章練習 2 加入的 `Phase` 欄位的版本也部署到 k3d，驗證 `kubectl get fooapp -o wide` 能看到 Phase 欄位。

提示：需要在 `FooApp` 的 struct 加上 printer column marker：

```go
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
```

---

[← 第 9 章：從零建立 Operator](./09-build-simple-operator.md) | [第 11 章：升級與異動機制 →](./11-upgrade-and-change.md)
