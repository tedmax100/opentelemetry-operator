# Stage 6：透過 OpAMP 遠端下推 Collector 版本升級

> **本階段目標：** Stage 5 只示範「讀」——server 看得到每個 collector 的生效設定。這階段補上「寫」：平台工程團隊透過一支 HTTP API 登記「這個 collector 該升到哪個版本」，opamp-server 在下一次收到該 collector 的回報時，把新版 `image` 用 RemoteConfig 推回去，Operator 管理的 bridge 收到後直接 `Update` CR，交由 Operator 的 reconcile loop 完成滾動升級——全程不需要 `kubectl apply`。

---

## 6.1 為什麼「改版本」這條路走得通

Stage 5 提過一個常見誤解：「OpAMP 只能改 pipeline 設定（receivers/processors/exporters），不能改 `replicas`/`image` 這種部署層欄位」。這是**協議語意上**該遵守的分工，但去看 `cmd/operator-opamp-bridge/internal/operator/client.go` 的實際實作，會發現它並沒有真的守住這條線：

```go
// cmd/operator-opamp-bridge/internal/operator/client.go:70-112
func (c Client) Apply(key string, configmap *protobufs.AgentConfigFile) error {
    ...
    var collector v1beta1.OpenTelemetryCollector
    yaml.Unmarshal(configmap.Body, &collector)   // 整個 CR spec，不只 pipeline
    err = c.validateComponents(&collector.Spec.Config)  // 白名單只檢查這三類
    ...
    return c.update(ctx, instance, updatedCollector)
}

func (c Client) update(ctx context.Context, o, n *v1beta1.OpenTelemetryCollector) error {
    n.ObjectMeta = o.ObjectMeta   // 只留舊 metadata
    n.TypeMeta = o.TypeMeta
    return c.k8sClient.Update(ctx, n)   // 整個 Spec 被取代，含 Spec.Image、Spec.Replicas
}
```

`componentsAllowed` 白名單（`validateComponents`，`client.go:115-146`）**只檢查 `Spec.Config` 裡的 receivers/processors/exporters**，完全沒管 `Spec.Image`。也就是說：只要 RemoteConfig 裡塞的是一份完整的新 CR spec（含新版 `image`），bridge 現有程式碼就會照單全收去 `Update`。

這既是這階段能實作版本升級的原因，也是一個值得記住的安全缺口——見 6.5 的閱讀理解題。

---

## 6.2 資料流

```
┌──────────────┐  POST /upgrade            ┌──────────────────────┐
│ 平台工程團隊  │  {key, image}             │   opamp-server        │
│ (kubectl / curl) ────────────────────────▶│   :8080 (admin HTTP)  │
└──────────────┘                            │   登記 pendingImage    │
                                             └──────────┬─────────────┘
                                                        │ 等下一次心跳
                                                        ▼
┌───────────────────┐   report(EffectiveConfig)  ┌────────────────────┐
│  lab-bridge        │ ─────────────────────────▶│  opamp-server       │
│  (Operator 管理)   │                            │  :4320 (/v1/opamp)  │
│                    │ ◀───────────────────────── │  比對 key 有無      │
│                    │   RemoteConfig(新 image)   │  pending image      │
└──────────┬─────────┘                            └────────────────────┘
           │ Client.Apply → k8sClient.Update(CR)
           ▼
┌────────────────────┐
│ OpenTelemetryCollector CR（gateway / agent / app-sidecar）
│ spec.image 已更新
└──────────┬─────────┘
           │ Operator reconcile loop（watch 到 CR 變化）
           ▼
   Deployment/StatefulSet/DaemonSet 滾動更新出新版 collector Pod
```

**為什麼不能主動 push，只能「等對方連線」？** OpAMP 是 agent 先連上 server 的協議（bridge 是 client，server 是 WebSocket server）。Server 沒辦法主動對外連線去推設定，只能在 `OnMessage` 的回應裡夾帶 `RemoteConfig`。所以 `/upgrade` 端點做的事只是「登記意圖」，真正送出去要等 bridge 下一次心跳（`heartbeatInterval`，預設頻率見 `OpAMPBridge` CR）。

---

## 6.3 程式碼改動

**`apps/opamp-server/main.go`** 新增：

- `state`：記憶體狀態，`effectiveConfigs`（每個 collector 最後回報的完整 CR YAML）+ `pendingImage`（還沒送出去的目標版本）
- `POST /upgrade`：body 是 `{"key": "otel-lab/gateway", "image": "otel/opentelemetry-collector-k8s:0.111.0"}`，`key` 格式跟 bridge 回報時一致（`namespace/name`，見 `cmd/operator-opamp-bridge/internal/operator/kube_resource_key.go`）
- `onMessage`：每次收到 `EffectiveConfig` 時，先把它存進 `effectiveConfigs`；如果這個 key 剛好有 pending image，就呼叫 `patchImage` 把 YAML 裡的 `spec.image` 換掉，包進 `ServerToAgent.RemoteConfig` 回傳，並算 `ConfigHash`（sha256）
- `patchImage`：用 `map[string]any` + `sigs.k8s.io/yaml` 解析/改寫，只動 `spec.image`，其餘欄位原封不動——因為 bridge 的 `update()` 是整份 spec 取代，漏改的欄位會被還原成 RemoteConfig 裡的值，不是保留原本 CR 上的值

> 為什麼用 `map[string]any` 而不是 import `apis/v1beta1` 的型別？保持這個教學用 server 獨立輕量，不用拉進整個 Operator 的 API 模組依賴。

Build：

```bash
cd classroom/lab
docker build -t opamp-server:lab ./apps/opamp-server
k3d image import opamp-server:lab --cluster otel-lab
kubectl -n otel-lab rollout restart deployment/opamp-server
```

`manifests/40-opampbridge.yaml` 也多開了 `containerPort: 8080` 與對應的 Service port（`admin`），沿用 Stage 5 已經部署過的話直接 `kubectl apply -f manifests/40-opampbridge.yaml` 即可。

---

## 6.4 觸發一次升級

先確認 `gateway` 目前的 image：

```bash
kubectl -n otel-lab get opentelemetrycollector gateway -o jsonpath='{.spec.image}{"\n"}'
```

Port-forward admin endpoint，登記升級：

```bash
kubectl -n otel-lab port-forward svc/opamp-server 8080:8080 &

curl -X POST localhost:8080/upgrade \
  -H 'Content-Type: application/json' \
  -d '{"key":"otel-lab/gateway","image":"otel/opentelemetry-collector-k8s:0.111.0"}'
# 預期：HTTP 202
```

看 server log，等 bridge 下一次心跳把 `gateway` 的 report 帶進來，應該會看到：

```
upgrade registered for "otel-lab/gateway" -> "otel/opentelemetry-collector-k8s:0.111.0" (下次收到它的 report 時會下推)
  effective-config[otel-lab/gateway]: ...
  >> pushing image upgrade for otel-lab/gateway -> otel/opentelemetry-collector-k8s:0.111.0
```

確認 CR 與 Pod 真的換了版本：

```bash
kubectl -n otel-lab get opentelemetrycollector gateway -o jsonpath='{.spec.image}{"\n"}'
kubectl -n otel-lab rollout status statefulset/gateway-collector
kubectl -n otel-lab get pods -l app.kubernetes.io/component=opentelemetry-collector -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].image}{"\n"}{end}'
```

---

## 6.5 練習 6

**閱讀理解題：** `validateComponents`（`client.go:115-146`）只檢查 `receivers`/`processors`/`exporters` 三個 key。如果一個惡意或設定錯誤的 OpAMP server，在 RemoteConfig 裡連 `metadata.namespace` 都改了（例如把 `gateway` CR「搬」到別的 namespace），bridge 端會發生什麼事？（提示：看 `update()` 只保留 `o.ObjectMeta`，這代表 `n.ObjectMeta` 会被整個蓋掉，包含 `Namespace`——再想想 `k8sClient.Update` 對一個「跟原資源 namespace 不同」的物件會回什麼錯誤。）

**動手題：** 目前 `patchImage` 只保護了「不要漏改欄位」，沒有限制「這個 image 是不是平台工程團隊核可的版本」。幫 `/upgrade` handler 加一個白名單（例如環境變數 `ALLOWED_IMAGES`，逗號分隔），拒絕不在清單裡的 image。

<details>
<summary>參考答案（閱讀理解題）</summary>

`update()` 執行 `n.ObjectMeta = o.ObjectMeta`，所以 `n` 最終的 `Namespace`/`Name` 一定跟原本查到的 `instance`（`o`）一致，**不會**因為 RemoteConfig body 裡塞了別的 `metadata.namespace` 就真的搬家——`Apply()` 一開始就是用 `key`（`namespace/name`）去 `GetInstance` 找到 `o`，RemoteConfig body 裡的 `metadata` 從頭到尾沒被採用。所以「改 namespace」這條路是安全的，被 `ObjectMeta` 覆蓋這個機制擋掉了。

但這也反過來說明：**唯一真正不受任何白名單保護、又會被完整套用的欄位，就是 `Spec` 底下除了 `Config` 以外的所有東西**——`Image`、`Replicas`、`Resources`、`Env` 等等。這就是本階段能做版本升級的原因，也是 6.5 動手題要補強的地方。

</details>

---

## 清理

跟 Stage 5 共用同一個 `otel-lab` namespace 與 cluster，清理方式見 [Stage 5 §清理整個 lab](./05-opamp-bridge-control-plane.md#清理整個-lab)。

---

| | |
|---|---|
| 上一步 | [← Stage 5](./05-opamp-bridge-control-plane.md) |
| 回到 | [README](./README.md) |
