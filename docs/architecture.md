# Fleet 架构设计

> 状态：草案 v0.1 · 讨论中未定稿的条目标 `[待定]`

## 1. 目标与非目标

**目标**：一个可开源的私有化大模型推理平台，两条主线——

1. OpenAI 兼容网关：鉴权、路由、限流、计费、配额
2. 模型部署与 GPU 资源管理：CRD 驱动的声明式部署、多节点推理编排、GPU 装箱

**非目标**（明确排除，避免范围蔓延）：

- 不做模型训练 / 微调平台
- 不做 RAG / 应用编排层
- 不做模型仓库分发（起步直接用 HuggingFace + 本地共享存储）
- 不做公有云多租户 SaaS 的支付/充值（内部记账单位即可）

## 2. 设计原则

这些是不可协商的，违反需要走 ADR。

### P1 · OpenAI 协议是唯一契约

`fleet-gateway` 与任何推理引擎之间只有 OpenAI 兼容 HTTP 协议。**代码里不 import 任何 vLLM/SGLang 的东西。**

推论：**新增引擎 = 新增一个 `engine.Profile` 字面量，不改任何接口。**（不是新增一个 Adapter —— 见 §5.1。）

### P1bis · 引擎多样性分三根轴，不分叉协议

这是 P1 被真正执行后的形状，也是本项目最容易被做错的一处设计。三根轴：

| 轴 | 是什么 | 数量 | 落在哪 |
|---|---|---|---|
| 怎么说话 | `engine.Adapter` | **1 个** | `core/internal/engine/openai` |
| 找什么 | `engine.Profile` | N 个，纯数据 | `core/internal/engine/profile.go` |
| 渲染成什么 | renderer | N 个 | operator 模块（尚未实现） |

`Adapter` **按协议**分，不按厂商分。引擎差异全部是 `Profile` 里的字面量：能加载什么权重格式、健康检查端点候选、要额外探测哪些扩展、指标路径与 series 名、最低 compute capability。

`Profile` 里的端点路径是**候选列表**而非单值，探测时逐个试。这是 P4 从"版本"推广到"端点名"的直接结论：把 `/health` 写死成一个字符串，上游改个名就变成"永远 not ready"，而这个故障从外面看像镜像拉不下来。写错一个候选的代价是一个 404，不是错的 `Capability`。

**硬门只有一个**：健康检查。其余全部软失败——`/tokenize`、`/version` 缺失只记录，不阻断。一个能 chat 但没有 `/tokenize` 的引擎完全可用，让整个探测失败等于把它踢出轮转。`llama-cpp` 的 `Tokenize` 候选列表**故意为空**，不做无用往返，也不谎称能精确计数。

`FleetDeploymentSpec` 里**不允许**出现 `if engine == "vllm"`。只有 `Engine` 字段 + 不透明的 `EngineOptions`。

### P2 · 横向扩副本 > 纵向堆卡

TP 的通信开销随卡数超线性增长，故障域随卡数线性膨胀。671B 模型 + 160 卡 → 10 个 16 卡 replica，不是 1 个 160 卡部署。

推论：单 replica 卡数上限 32（8 卡节点 × 4），甜区在 16。数据模型围绕 `Replica` 而非 `Shard` 组织。

### P3 · Gateway 拥有 endpoint 选择权

路由决策需要三个上下文，只有 gateway 同时具备：token 计数、租户配额、成本池水位。

推论：不使用 Envoy / Gateway API Inference Extension 做 LLM 路由。Envoy 只做南北向入口，不进 AI 路径。

### P4 · 自有 CRD，不 adopt 上游 CRD

`FleetDeployment` / `FleetModel` / `FleetCluster` 是我们自己的 API。controller 内部渲染成 KubeRay `RayCluster` 或 AIBrix `StormService`。

推论：上游组件（KubeRay / AIBrix / KAI Scheduler）的版本升级不会成为我们的 API 破坏性变更。

### P5 · 限流必须硬性预留，不能事后扣减

事后扣减在并发下必然超支。网关在转发前原子预留 `max(prompt_estimate, 1) + max_tokens`，结算时按实际 usage 补差额或退回。

### P6 · 不信任客户端上报的 usage

计费的权威来源是引擎返回的 `usage` 字段。客户端请求体里的任何数字都不参与计费。流式中断拿不到 usage 时按 `max_tokens` 封顶并打 `estimated` 标记，事后对账修正。

### P7 · Gateway 与 Kubernetes 解耦

`core` 模块不依赖任何 k8s 库。这不是洁癖：gateway 镜像要小、启动要快、贡献者不该为了改一个 handler 而下载整个 k8s.io。

推论：**两个 Go module**。`core`（无 k8s 依赖）+ `operator`（依赖 k8s + core）。边界由编译器强制，不靠 code review 自觉。

### P8 · 计费是成本分摊，不是余额扣减

见 §6。

## 3. 系统总览

```
                     ┌──────────────────────────────────────┐
   OpenAI SDK ──────▶│  fleet-gateway  (Go, 无 k8s 依赖)     │
                     │                                      │
                     │  auth → ratelimit → routing → proxy   │
                     │                    ↓          ↓     │
                     │              usage tap   engine.Adapter│
                     │                    ↓          ↓     │
                     └────┬────────────────┘          └─────┘
                          │ UsageEvent                      │ OpenAI 协议
                          ▼                                 ▼
              ┌──────────────────┐              ┌────────────────────────┐
              │ Redis (限流/预留) │              │ vLLM / SGLang replicas │
              │ Postgres (账本)   │              │ (K8s, headless svc)   │
              │ ClickHouse (用量) │              └───────────┬────────────┘
              └────────▲─────────┘                          │
                       │                            ┌─────────▼──────────┐
   ┌───────────────────┴──────────┐                 │  fleet-controller   │
   │ fleet-apiserver (控制台 API)  │                 │  (Go, k8s operator) │
   └──────────────────────────────┘                 └─────────┬──────────┘
                                                               │ 渲染
                                    ┌──────────────────────────┼──────────────────┐
                                    ▼                          ▼                  ▼
                             RayCluster              StormService          KAI Scheduler
                            (KubeRay)               (AIBrix)           (gang + 拓扑)
```

### 请求路径

```
1. auth        API key → tenant（带缓存，本地 LRU + Redis 回源）
2. ratelimit   Redis 原子扣减配额，预留 max_tokens
3. route       model + tenant → endpoint 集合 → 一致性哈希选一个
4. proxy       SSE 透传，边转发边 tap
5. usage tap   解析 SSE 末帧的 usage；无则按已收 chunk 估算
6. settle      差额结算 / 退回预留，写 UsageEvent
7. async       UsageEvent → ClickHouse；账本 → Postgres
```

## 4. 模块划分

依赖方向严格单向：`pkg` ← `core/internal` ← `cmd`。`operator` 单向依赖 `core`。

### `core`（module `github.com/zlogic-labs/fleet`，零 k8s 依赖）

| 包 | 职责 | 关键类型 |
|---|---|---|
| `pkg/errs` | 分级错误 + HTTP 映射 | `Error{Kind, Code}` |
| `pkg/ptr` | 指针辅助（nullable 列扫描） | `To`, `From` |
| `pkg/tokenizer` | 预检计数，三级回退 | `Tokenizer` |
| `pkg/log` | slog 上下文封装 | `Logger` |
| `internal/config` | 配置加载（env + yaml） | `Config` |
| `internal/engine` | 引擎抽象：Adapter（一个）+ Profile（数据） | `Endpoint`, `Capability`, `Profile`, `Adapter` |
| `pkg/weights` | 权重格式分类，决定谁能加载 | `Format`, `Of` |
| `internal/gateway/routing` | endpoint 选择（一致性哈希 + 健康） | `Picker`, `HealthTracker` |
| `internal/gateway/transport` | SSE 透传 + usage tap | `Proxy`, `Tap` |
| `internal/gateway/auth` | 鉴权与配额上下文 | `Resolver`, `Principal` |
| `internal/gateway/ratelimit` | 分层限流 | `Limiter` |
| `internal/gateway/handler` | OpenAI 端点 handler | — |
| `internal/gateway/usage` | UsageEvent 采集与落库 | `Sink` |
| `internal/store/postgres` | 领域仓储 | 各 repository |
| `internal/store/clickhouse` | 用量分析写入 | `Writer` |
| `internal/billing` | 计价、账本、成本分摊 | `Pricer`, `Ledger` |
| `internal/apiserver` | 控制台 REST API | — |

### `operator`（module `.../fleet/operator`）

| 包 | 职责 |
|---|---|
| `api/v1alpha1` | CRD Go 类型（controller-gen 生成 deepcopy） |
| `internal/controller` | 各 CRD 的 reconciler |
| `internal/render` | FleetDeployment → RayCluster / StormService |
| `internal/scheduler` | GPU 装箱、拓扑匹配、gang 分配 |

## 5. 核心数据模型

### 5.1 引擎能力：Adapter 只有一个，Profile 是数据

```
core/internal/engine/engine.go      Adapter 接口（按协议）· Capability · Endpoint
core/internal/engine/profile.go     Profile · ProfileRegistry · vllm / llama-cpp 两个字面量
core/internal/engine/openai/        唯一实现：probe 走 Profile 的候选列表
core/pkg/weights/                   Format 分类（safetensors / gguf / unknown）
```

`engine.Profiles.For(name)` 永不失败：没登记的引擎拿到 `DefaultProfile()`，它**只**假设 OpenAI 兼容面，不声明格式、不声明指标。少假设是重点——对未知引擎的猜测会变成错的 readiness 判定，而 readiness 决定流量往哪走。查找带家族前缀回退（`vllm-0.9.1` 仍命中 vLLM），因为拼写差异静默丢掉格式检查会放 GGUF 进 vLLM。

`pkg/weights` 独立成包是因为两个无关层需要同一个答案而都不拥有它：registry 记录拉到了什么，engine 判断谁能加载。放任何一边都会造出反向依赖。

**权重格式必须存进 registry 条目。** safetensors 和 GGUF 不是同一模型的两种文件，是任何给定引擎只能读其中一种的两种模型。GGUF 仓库没有 `config.json`，只检查 `config.json` 会把 llama.cpp 模型报成 ready 然后加载失败。

`RequiredFiles` 是**候选组**不是扁平列表：每组至少命中一个。safetensors 仓库要么是单个 `model.safetensors`，要么是 index + 若干 shard，二者互斥，扁平"全部必需"会把正确的仓库报成不完整。

`MinCompute` 让不可能的组合在 admission 阶段几秒内被拒，而不是永远 pending。vLLM 官方要求 compute capability 7.5+，GT 720（CC 3.5）和 1060（CC 6.1）都在门外；llama.cpp 的 `MinCompute` 是 0，因为它仍支持 Pascal。这些数字会漂移，用途是快速失败常见情况，不是权威——权威是模型能不能加载，只有真跑一次能回答。

### CRD

```go
// FleetModel — 逻辑模型，与部署解耦
type FleetModelSpec struct {
    Source       string   // HF repo 或本地路径
    Format       string   // safetensors | gguf；由拉取时看到的文件推断，不要求人填
    TokenizerID  string   // tiktoken 编码名；空则走估算。GGUF 恒为空
    ContextLimit int32
    PricingRef   string   // 指向 PriceBook
}

// FleetDeployment — 一次可调谐的部署
type FleetDeploymentSpec struct {
    ModelRef   string
    Replicas   int32                    // 横向副本数（P2）
    TensorParallelSize int32            // ≤ 单节点卡数
    PipelineParallelSize int32
    Engine     string                   // 查 engine.Profiles，不是 switch 的分支
    EngineOptions map[string]string     // 版本无关的声明式覆盖
    Resources  GPURequest              // {count, model, vramGB, interconnect}
    Autoscaling AutoscalingSpec
}

// FleetCluster — 多集群注册
type FleetClusterSpec struct {
    Endpoint, KubeconfigRef, Scheduler string  // scheduler: default | kai
}
```

`admission` 阶段用 `engine.Compatible(model.Format, spec.Engine)` 拒绝格式不匹配的组合，并给出可读原因。控制台用同一个函数的反方向（`engine.EnginesFor`）把不能用的引擎置灰，而不是接受组合后在 admission 失败——那时候调度往返已经花掉了。

### PostgreSQL

| 表 | 说明 |
|---|---|
| `tenants` | 租户 |
| `api_keys` | key 哈希、租户、scope、配额上限 |
| `models` | 模型注册表（对应 FleetModel） |
| `deployments` | 部署状态（controller 写，gateway 读） |
| `endpoints` | 运行中的 vLLM 实例：ready、load、affinity 标签 |
| `clusters` / `nodes` / `gpus` | GPU 库存，`gpus` 含 uuid / 型号 / 显存 / 互联拓扑 |
| `price_books` | 计价：per 1M tokens，分 in / out / cached |
| `usage_events` | 权威用量（高量走 ClickHouse，Postgres 只留账本） |
| `ledger_entries` | **权威账本**，只增不改 |
| `wallets` / `balances` | 内部记账单位余额 |
| `rate_limit_policies` | 限流策略 |

### ClickHouse

`usage_events` 明细，按 `(tenant_id, model, toDate(ts))` 排序，用于用量分析和成本报表。Postgres 存聚合后的账本，ClickHouse 存明细，**对账任务比对两者**。

## 6. 计费模型（核心差异化）

这是与 New API / LiteLLM 类项目**根本不同**的地方。

### 成本结构

| | API 中转商 | Fleet |
|---|---|---|
| 成本性质 | 变动，随用量线性增长 | **固定**，卡买了就是买了 |
| 闲置时 | 无成本 | **闲置也在烧钱** |
| 成本中心 | 上游单价 | 卡折旧 + 电费 + 机架 + 网络 |
| 毛利来源 | 价差 | **效率优化**（prefix cache / autoscaler / MIG / P/D 分离） |

### 记账流程

```
1. 成本池     月初按集群折旧+电费+机房 计算当月固定成本 C_pool
2. 占用       每个 replica 的 GPU·小时累计到 pod_usage 表
3. 分摊       租户 T 的成本 = C_pool × (T 的 GPU·小时 / 全平台 GPU·小时)
4. 归一       换算成 token 的"内部计价单位"，让用户看到熟悉的用量口径
5. 闲置摊销   未被任何租户占用的部分，按"预留未使用"分摊给按需租户
```

**闲置率是这个平台的第一 KPI**，不是 QPS。它同时是 autoscaler 的经济驱动力和成本报表的透明度指标。

### 从中转商项目借鉴的模式

只取两个，其余不要：

- `micro-one-api` 的**预扣 → 释放 → 结算**（处理流式请求失败时的额度回滚）
- `Octafuse` 的**三账本**（供应成本 / 目录价 / 用户计费分离，便于算毛利和分账）

## 7. 路由与缓存亲和

### 为什么不能轮询

vLLM 的 KV cache 按 prompt 前缀复用。轮询把前缀打散 → 命中率归零 → 同样的 GPU 吞吐掉一个数量级。llm-d 在 MI300X 上的实测：开启 prefix-cache 感知路由后输出 token/s 3x、TTFT 减半。

### 策略

```go
// key = sha256(system_content + 前 N token)，N 可配（默认 512）
// → 虚拟节点 → 端点的一致性哈希
// 候选集内：过滤不健康 → 按 (KV cache 命中率, 队列深度) 打分 → 取最优
```

熔断按 `engine` + `endpoint` 两级计数，连续失败进入冷却。冷却期内不参与打分，但**健康探测继续**——避免"恢复后仍被标记不健康"的死锁。

## 8. 依赖清单

### 运行时依赖（刻意精简）

| 依赖 | 用途 | 为什么不选别的 |
|---|---|---|
| `go-chi/chi/v5` | HTTP 路由 | 比 gin 轻，stdlib 兼容，无全局状态 |
| `samber/lo` | 函数式切片/映射操作 | 消除大量 `for range + append` 样板 |
| `jackc/pgx/v5` + `sqlc` | Postgres 访问 | SQL 可见，计费这类需要精确 SQL 的地方 ORM 会添乱 |
| `redis/go-redis/v9` | 限流、配额预留 | Lua 脚本原子操作 |
| `golang.org/x/time/rate` | 单机令牌桶 | 挡突发，不做分布式配额 |
| `pkoukk/tiktoken-go` | OpenAI 系 tokenizer 计数 | 纯 Go，无 cgo |
| `prometheus/client_golang` | 指标 | 生态标准 |
| `go.opentelemetry.io/otel` | 链路追踪 | vLLM/KServe 都能导出 OTLP |
| `golang.org/x/crypto` | 密码哈希 | |
| `google/uuid` | ID | |
| `gopkg.in/yaml.v3` | 配置 | 不引 koanf，配置结构简单不值得多一个依赖 |

### operator 依赖

`sigs.k8s.io/controller-runtime`、`k8s.io/{api,apimachinery,client-go}`、`sigs.k8s.io/yaml`

KubeRay / AIBrix 的 CRD **用 `unstructured` 消费**，不引它们的 Go client——它们的类型定义跟着上游 release 走，引进来就是版本地雷。

### 代码生成

| 工具 | 产出 |
|---|---|
| `sqlc` | `core/internal/store/postgres/*.sql.go` |
| `controller-gen` | `operator/api/v1alpha1/zz_generated.deepcopy.go` + CRD yaml |
| `goimports` | 格式化 |

## 9. 本地开发环境

k3s + WSL2，**仅用于控制面开发**。一键脚本：`deploy/k3s-dev/setup.sh`，约束与验证边界见 `deploy/k3s-dev/README.md`。

- WSL2 必须开 systemd（`/etc/wsl.conf` → `[boot] systemd=true`），k3s 装不上 WSL1
- **NVIDIA GPU Operator 在 WSL2 不可用**。它要装 `nvidia.ko` 内核模块，而 WSL2 用的是 Windows 驱动的 paravirtualized 实现，这条路是堵死的。本地直接跑 `nvidia/k8s-device-plugin` DaemonSet
- **MIG 在 WSL2 不支持**——MIG 逻辑只能靠 CI 上的真 Linux 机器验证
- **NVML 在容器内经常枚举不到 GPU**。即使宿主 `nvidia-smi` 正常，插件也会注册 socket 但报告 0 个设备，节点不上报 `nvidia.com/gpu`。这是环境限制，不是代码问题
- device plugin 的 Pod 必须显式指定 `spec.runtimeClassName: nvidia`。上游静态清单把它放在默认 runtime 上，容器内没有 NVML，于是"注册成功但 0 设备"——这个坑排查起来很费时间
- containerd 2.x 下 `nvidia-ctk runtime configure` 写的是 `/etc/containerd/conf.d/99-nvidia.toml`，不是传入的 `--config` 路径。k3s 的 config 有 `imports = [... "/etc/containerd/conf.d/*.toml"]`，所以能生效，且 `runc` 仍是默认 runtime
- CNI 用默认 flannel，不要 Cilium（WSL2 内核 eBPF 支持不全）
- 模型权重必须放在 WSL 文件系统内，**不能放 `/mnt/c`**（9p 协议，safetensors 加载慢一个数量级）。从 `/mnt/d` 构建源码是可以的——Go 的 build cache 在 ext4 上，真正耗时在那里，当前 tree 冷构建约 20 秒
- k3s 默认 Traefik 保留作南北向入口

**测试边界**：控制面逻辑（CRD 生命周期、rollout、扩缩容决策、网关全链路）本地全覆盖；GPU 特有逻辑（MIG、拓扑感知放置、多节点 TP、NVML 枚举）只在 CI 真机验证。

## 10. 实施顺序

| 阶段 | 内容 | 可验证标准 |
|---|---|---|
| 1 | `pkg/*` + `internal/engine` + gateway transport | curl 打到 mock endpoint，SSE 完整透传，usage 能取到 |
| 2 | auth + 限流 + 配额预留/结算 | 超限返回 429，预留正确回滚 |
| 3 | routing：一致性哈希 + 健康 | 同前缀命中同端点，熔断能恢复 |
| 4 | 计价 + 账本 + ClickHouse | 账实一致，对账任务能跑 |
| 5 | operator：CRD → RayCluster | k3s 上 `kubectl apply` 能起一个 vLLM |
| 6 | 成本分摊 + 控制台 | 成本报表数字对得上 |
| 7 | autoscaling + 调度器 | 队列深度驱动扩缩，无抖动 |

阶段 1–4 全部不依赖 Kubernetes，可以纯 Go 单测覆盖。**这是把模块边界划在 P7 的直接收益。**

## 11. 待定

- `[待定]` 是否第一版就支持 Anthropic 原生协议，还是只做 OpenAI 兼容 + 一个转换层
- `[待定]` 成本池的计价周期（自然月 vs 滚动窗口）与跨周期欠款处理

### 11.1 权重的分发策略不是一个全局开关

原来这条是"共享存储（GPFS/Lustre）vs 节点本地 NVMe + 预热"二选一。引入 `weights.Format` 之后**问题变了**：策略随格式走，所以不该是一个全局选择。

| 格式 | 典型体积 | 合理分发 |
|---|---|---|
| safetensors | DeepSeek-R1 671B = 1.3 TB | **只能**共享存储。复制到节点在物理上不成立。 |
| GGUF（Q4） | 0.5B–70B = 0.4–45 GB | 复制到节点本地 NVMe + 预热可行，冷启动从分钟级降到秒级。 |

也就是说：GGUF 部署的 autoscaler 冷启动可以做到可接受，safetensors 671B 部署的冷启动本质上是"加载 1.3 TB"的时间，扩缩容策略必须承认这个事实而不是假装可以预热。

这直接影响 P/D 分离是否可行，也影响 `FleetDeploymentSpec` 要不要显式声明 `WeightDelivery: shared | nodeLocal`。**倾向**：显式声明，operator 据此选 init container 还是直接挂载——但两条路径都还在 operator 里，尚未实现。

## 12. 社区版与企业版

### 12.1 唯一的硬性规则：不 fork

企业版仓库里**不允许包含 core 的任何一份修改副本**。这不是风格偏好，是可执行约束：

```
fleet/core/                 Apache 2.0，公开
  pkg/entitlement/           ← 接缝在这里定义
  pkg/authn/                 接口 + 社区实现（本地账号）
  pkg/audit/                 接口 + 社区实现（结构化日志）
  ...
fleet/enterprise/           私有，独立仓库
  引用 core，不修改 core
  提供 *另一些* 实现 + 一份签名 license 文件
```

一旦 fork 出现，"社区版和企业版行为一致"就再也无法验证，两边的 bug 修复会以指数速度分叉，而这个平台的客户正是最不能容忍分叉的那类人。

### 12.2 分的是 entitlement，不是代码

每个可能被 gate 的能力是一个 `entitlement.Capability` 常量，由一处检查：

```go
if err := lic.Require(entitlement.CapAudit, time.Now()); err != nil {
    return errs.New(errs.KindPermissionDenied, "feature_requires_enterprise", ...)
}
```

社区构建的 `entitlement.Community()` 不 grant 任何能力。企业构建读签名 license 文件。**两者跑同一份代码**，区别只有 license 文件的内容。

能力清单（7 项，`core/pkg/entitlement/entitlement.go`）：`sso`、`audit`、`rbac`、`policy`、`multicluster`、`ha`、`cost_export`。

商业逻辑上这 7 项的共同点是：**它们全部服务于"把平台交给别人管"，而不是"把模型跑得更快"。** 闲置率、路由、计费精度这些真正难的东西全部留在社区版——留在这里才有人用，社区才有人贡献，企业版才有人买。

### 12.3 不做两套页面

一个前端，按 entitlement 显示/隐藏。理由是维护成本而非偷懒：两套页面意味着每个 UI 改动要写两遍，两遍会漂移，而漂移出来的不一致会被销售当成 bug 报上来。

`GET /fleet/status` 直接返回当前 edition 与已 grant 的能力列表，控制台据此渲染 Entitlements 面板——所以"你买的是什么"是服务端的事实，不是浏览器的一个 CSS 类。删掉页面上那个徽章不会解锁任何东西。

### 12.4 许可证失效的降级

`License.Expired` 之后**回落到社区版能力，而不是拒绝服务**。让客户因为发票过期而读不到自己的成本数据，是在最不该失败的时候制造故障；回落到社区版恰好也是有效的销售信号。
