# 104 現行 OTel Collector(Helm)→ Operator 遷移參考

把 `k8s-gitops-infra-rancher/apps/open-telemetry` 現行的四個 helm release
改寫成 `OpenTelemetryCollector` CR 的完整參考。**以實際部署的 `.gen.yaml` 為準**
(部分 values 檔與 gen 檔已不同步,見下方「發現的問題」)。

## 現況 → 目標對照

```
現況(每個 release 一份 values + helm template 出來的 gen.yaml)
  collector        Deployment+ConfigMap+Service+HPA+SA+Ingress+ServiceMonitor
  span-lb          同上 + ClusterRole/Binding(k8s resolver)
  span-collector   同上
  metrics-lb       同上(overlay-only,contrib image)

目標(operator 管)
  base/collector.yaml        ← 一個 CR,operator 生出上面那一整包
  base/span-lb.yaml
  base/span-collector.yaml
  base/metrics-lb.yaml
  base/rbac.yaml             ← SA(掛 imagePullSecrets)+ resolver 用的 ClusterRole(手寫)
  base/compat-services.yaml  ← 舊名 Service alias,應用端 endpoint 不用改
  base/servicemonitors.yaml  ← collector 的 8888/8889 抓取(8889 要 relabelings,只能手寫)
  base/ingress.yaml          ← 手寫(operator 的 spec.ingress 會改變對外 path)
  overlays/lab-cluster/      ← per-cluster patch 直接打 CR,不再 patch render 產物
```

資料流不變:

```
app ─OTLP─> span-lb ─(traceID LB)─> span-collector ─tail_sampling─(traceID LB)─> tempo
app ─OTLP─> metrics-lb ─(streamID LB)─> collector ─deltatocumulative─> prometheus exporter :8889
```

## 前置條件

1. 每個 cluster 安裝 cert-manager + opentelemetry-operator(建議 OLM 或 helm chart `opentelemetry-operator`)
2. prometheus-operator CRD(現況已有,ServiceMonitor 在用)
3. namespace `open-telemetry` 與 secret `image-pull-secret` 已存在

## 遷移後的日常操作變化

| 事項 | 現況 | Operator 後 |
| - | - | - |
| 改 pipeline 設定 | 改 values → `helm template` 重 render → commit gen.yaml | 直接改 CR 的 `spec.config`,commit |
| config 生效 | ConfigMap 更新後要自己滾 pod | operator 以 config hash 自動滾動重啟 |
| 設定打錯 | apply 成功、pod CrashLoop 才發現 | webhook 在 apply 當下就 reject |
| per-cluster 差異 | patch Deployment/HPA/ConfigMap 三處 | patch CR 一處(見 overlays/lab-cluster) |
| 升級 collector 版本 | 改 values 的 image.tag + 重 render | 改 CR 的 `spec.image`(自訂 distro 維持 pin) |

## chart 幫你做、operator 不做的事(已在檔案裡補上)

1. **`MY_POD_IP` env**:chart 自動注入,operator 不會 → 每個 CR 的 `spec.env` 手動加 fieldRef。
2. **imagePullSecrets**:CR 無此欄位 → 掛在自訂 SA(`rbac.yaml`),CR 用 `spec.serviceAccount` 指定。
3. **GOMEMLIMIT**:chart 自動算 80% memory limit → CR 裡寫死字面值(改 memory limit 時記得同步改)。
4. **k8s resolver 的 RBAC**:chart 的 `clusterRole.create` → 手寫 `rbac.yaml`(operator 不會為
   loadbalancing exporter 產 RBAC),並補上 0.14x 需要的 `endpointslices`。
5. **ServiceMonitor 客製**:operator 的 `enableMetrics` 不支援自訂 interval/relabelings →
   collector 的 8889(60s + labeldrop)手寫;其他三隻只要 self-telemetry,用 `enableMetrics: true`。
6. **Ingress path 語意**:`spec.ingress`(path 型)會生 `/<port名>` 路徑,現況是 `/` 直通 4318 →
   Ingress 維持手寫,backend 改指 `<CR名>-collector` Service。

## 命名切換(遷移期最大的坑)

operator 產出的資源一律叫 `<CR 名>-collector`:

| 舊 Service | 新 Service |
| - | - |
| `collector` | `collector-collector` |
| `span-lb` | `span-lb-collector` |
| `span-collector` | `span-collector-collector` |
| `metrics-lb` | `metrics-lb-collector` |

`compat-services.yaml` 用舊名建 alias Service(selector 指向 operator 管的 pod),
讓應用程式端 endpoint 和 lb 的 resolver 設定都不用改。但**舊 helm release 還在時同名 Service
會衝突**,切換順序建議:

1. 裝 operator,apply 本目錄(先「不含」compat-services.yaml)→ 新舊兩套並存,各有各的 Service
2. 用新 Service 名驗證資料流(grpcurl 打 `span-lb-collector:4317`,Tempo/Prometheus 查得到)
3. 移除舊 helm release(舊名 Service 隨之消失)
4. apply compat-services.yaml → 應用端無感切換
5. (可選)之後把應用端逐步改指新名,再刪 alias

## 刻意不搬的東西

- **`hostPort: 4317/4318`**:現況三隻 deployment 都開了 hostPort,這會讓不同 collector 的 pod
  無法同節點共存(4317 撞 port),疑似是沿用範本的非刻意設定,CR 裡不搬。若確定需要,
  `spec.ports` 有 `hostPort` 欄位可以加回來。
- **`livenessProbe`/`readinessProbe`**:operator 會從 config 的 `health_check` extension 自動配,不用手寫。
- **`replicas: 1`**:有 `spec.autoscaler` 時由 HPA 決定,不需要。

## 發現的問題(遷移前值得先修)

- `base/collector.values.yaml` 與 `base/collector.gen.yaml` **不同步**:gen 檔的 batch 有調參
  (`timeout: 2s, send_batch_size: 8192, send_batch_max_size: 10240`)、`deltatocumulative.max_stale`
  是 `2m`(values 寫 10m)、pipeline 順序是 `memory_limiter → deltatocumulative → batch`
  (values 是 batch 在前)。本參考以 gen(實際部署)為準——這正是 render 式流程的風險,
  operator 化之後 CR 即真相,不會再發生。
- lab overlay 手寫的 gRPC ingress 帶著過期的 helm labels(chart 0.101.2 / 0.142.0 混雜),
  本參考已拿掉。

## 兩種管理方式(擇一)

本目錄提供兩條等價的路,render 出來的 CR 內容完全相同:

**方式一:kustomize 直接管 CR**(`base/` + `overlays/`)——CR 就是 git 裡的檔案,
沒有 render 步驟,per-cluster 差異用 kustomize patch。

**方式二:薄 wrapper chart**(`chart/`)——保留現行「改 values → `helm template` →
commit gen 檔 → Fleet apply」的工作流程,只是 render 產物從 Deployment 變成 CR。
templates 把共通機制自動化:`MY_POD_IP` 注入、GOMEMLIMIT 從 limits 算 80%、
SA 掛 pull secret、resolver RBAC、客製 ServiceMonitor、Ingress。

```bash
# 對照現行 Makefile 的 render 流程
cd chart
printf 'collector\nspan-lb\nspan-collector\nmetrics-lb' | xargs -n1 bash -c \
  'helm template $0 . -f values/$0.values.yaml --namespace open-telemetry > $0.gen.yaml'
```

per-cluster 差異 = 多疊一份小 values(helm 的 `-f` 後蓋前),例如 lab 的 span-collector:

```yaml
# overlays/lab/span-collector.values.yaml(整份就這樣)
extraEnvs:
  - name: TAIL_SAMPLING_PERCENTAGE
    value: "100"
resources:
  requests: {cpu: 250m, memory: 256Mi}
  limits: {cpu: "1", memory: 512Mi}
autoscaler: {minReplicas: 1, maxReplicas: 12, targetCPUUtilization: 300, targetMemoryUtilization: 100}
```

```bash
helm template span-collector . -f values/span-collector.values.yaml \
  -f overlays/lab/span-collector.values.yaml -n open-telemetry > span-collector.gen.yaml
```

注意:helm 對 list(如 `extraEnvs`、`autoscaler` 是 map 沒問題)是整段取代不是合併,
覆寫時要寫完整;`config` 是 map,可只寫要改的 key 做深合併。

## 驗證

```bash
# 方式一
kubectl kustomize overlays/lab-cluster   # 純 render 檢查
# 方式二
helm template collector chart -f chart/values/collector.values.yaml -n open-telemetry

kubectl apply --dry-run=server -f <render結果>   # operator webhook 會驗 spec.config
kubectl get otelcol -n open-telemetry            # READY 欄位確認 reconcile 成功
```
