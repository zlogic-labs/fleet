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
| 怎么说话 | `engine.Adapter` | **1 个** | `core/pkg/engine/openai` |
| 找什么 | `engine.Profile` | N 个，纯数据 | `core/pkg/engine/profile.go` |
| 渲染成什么 | renderer | N 个，注册进 `render.Registry` | fleet-serving 的 `internal/render` |

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

`FleetDeployment` / `FleetModel` 是我们自己的 API。controller 内部渲染成 K8s 原生对象（`Deployment` + `Service`），必要时渲染 gang 调度用的 PodGroup。

推论：上游组件（KubeRay / AIBrix / KAI Scheduler）的版本升级不会成为我们的 API 破坏性变更。

#### 为什么不用 KubeRay（2026-10-01 核实）

**这个结论会过期，所以先记日期和依据。**

vLLM `main` 分支 `vllm/config/parallel.py` 里有硬约束：

```python
allowed_backends = ("mp", "uni", "external_launcher")
if self.distributed_executor_backend not in allowed_backends and self.nnodes > 1:
    raise ValueError("nnodes > 1 can only be set when distributed executor "
                     "backend is mp, uni or external_launcher.")
```

**`nnodes > 1` 明确拒绝 ray。** vLLM 走的是 K8s 原生路线：`external_launcher`（靠 `RANK`/`WORLD_SIZE` 环境变量，每 rank 一个 Pod）、或 `mp` + `--nnodes` + `--data-parallel-master-ip`；`data_parallel_external_lb` 的文档原话是 *"useful for a 'one-pod-per-rank' wide-EP setup in Kubernetes"*。

（一次更正：`vllm/executor/ray_executor.py` 返回 404 不是删除，是搬到了 `vllm/v1/executor/`，`parallel.py` 里仍有 `from vllm.v1.executor import ray_utils`。但上面那条约束是真的。）

所以 KubeRay 不是我们的底座，它只是"如果要渲染 RayCluster 时的另一个 renderer"。真要用，加一个 renderer 即可，不改架构。

Ray Serve LLM 也不适合做在线 serving 底座：它自带 prefix 感知路由器，而 P3 规定路由权归网关（唯一同时掌握 token 计数、配额、成本池水位的一方）。它适合训练 / 批量 / RL 线的后端。

### P5 · 限流必须硬性预留，不能事后扣减

事后扣减在并发下必然超支。网关在转发前原子预留 `max(prompt_estimate, 1) + max_tokens`，结算时按实际 usage 补差额或退回。

结算的实现细节比这条承诺本身更容易写错，值得记下来：

- **整笔撤除再记实际值，而不是退差额。** 引擎报的比预留多（模型忽略了 `max_tokens`，或 prompt 比估算长）时，"退差额"会退成一个负数，于是原始扣减留在原地**并且**又加上了实际值，等于收两次。撤掉全部预留、单独记实际值，两个方向都对。
- **settle 到 0 要把请求数也还回去。** 请求没到引擎（路由没命中、副本拒了）就不该消耗配额；只退 token 不退 request 次数的话，打错一个模型名就能烧掉一整分钟的请求额度。
- **预留发生在 `Pick` 之前**，超限的请求连一次调度决策和一条连接都不该产生。`Pick` 失败必须 settle 成 0，否则每个 404 都会漏掉整笔预留。
- **滑动窗口，不是固定窗口。** 固定窗口的边界是可以瞄准的：第 N 分钟最后 1 秒花光额度、第 N+1 分钟开头再花一次，两秒内拿到两份。环形桶按秒复用，过期只由 `live` 判定，不需要清扫 goroutine。
- **已结算的量必须跟预留一样随窗口过期。** 把 settled 记成一个只增不减的标量，会把"每分钟 token 上限"变成"终身 token 上限"：租户花满一次额度后被永久拒绝，只能重启网关。实测：花 600/1000 后推进两个窗口，仍然 429。

### P6 · 不信任客户端上报的 usage

计费的权威来源是引擎返回的 `usage` 字段。客户端请求体里的任何数字都不参与计费。流式中断拿不到 usage 时按 `max_tokens` 封顶并打 `estimated` 标记，事后对账修正。

### P7 · Gateway 与 Kubernetes 解耦

`core` 模块不依赖任何 k8s 库。这不是洁癖：gateway 镜像要小、启动要快、贡献者不该为了改一个 handler 而下载整个 k8s.io。

推论：**两个 Go module，两个仓库**。`core`（无 k8s 依赖，主仓 `zlogic-labs/fleet`）+ `fleet-serving`（依赖 k8s + core，`zlogic-labs/fleet-serving`）。边界由编译器强制，不靠 code review 自觉。

拆成两个仓库而不是一个仓库两个目录，是因为拆仓前唯一需要钉死的就是两仓之间的那条契约。放在一起时它是个"顺手改一下就行"的内部调用；分仓之后它是公开接口，改它要发两个版本、放两个 release note。**在还便宜的时候定契约，比在已经依赖它的时候定便宜。**

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
              ┌───────────────────────────┐         ┌────────────────────────┐
              │ Postgres (限流/配额/账本)  │         │ vLLM / llama.cpp       │
              │ ClickHouse (用量明细)     │         │ replicas (K8s, headless)│
              └────────▲──────────────────┘         └───────────┬────────────┘
                       │                            ┌─────────▼──────────┐
   ┌───────────────────┴──────────┐                 │  fleet-controller   │
   │ fleet-apiserver (控制台 API)  │                 │  (Go, k8s operator) │
   └──────────────────────────────┘                 └─────────┬──────────┘
                                                               │ 渲染
                                    ┌──────────────────────────┼──────────────────┐
                                    ▼                          ▼                  ▼
                        Deployment + Service           (P/D 分离时)         PodGroup
                          (vLLM / llama.cpp)             成对的 Pod 组      (gang + 拓扑)
```

**渲染目标是 K8s 原生对象，不是 RayCluster**——理由见 P4 的核实记录。TP=8 渲染成 8 个独立 Pod 会被默认调度器**拆开调度**（3 个落 3 个节点，5 个 Pending，模型永远起不来），所以 TP>1 时需要 gang 调度：倾向 scheduler-plugins 的 coscheduling（对 Pod 形状中立），备选 Volcano / KAI Scheduler。

### 请求路径

```
1. auth        API key → tenant + project（缓存，回源到 Postgres）
2. ratelimit   预留 prompt + max_tokens；租户行锁下扣减，滑动窗口
3. route       model + tenant → endpoint 集合 → 一致性哈希选一个
4. proxy       SSE 透传，边转发边 tap
5. usage tap   解析 SSE 末帧的 usage；无则按已收 chunk 估算
6. settle      差额结算 / 退回预留，写 UsageEvent
7. async       UsageEvent → ClickHouse；账本 → Postgres
```

## 4. 模块划分

依赖方向严格单向：`pkg` ← `core/internal` ← `cmd`。`fleet-serving` 单向依赖 `core`。

### `core`（module `github.com/zlogic-labs/fleet/core`，零 k8s 依赖）

| 包 | 职责 | 关键类型 |
|---|---|---|
| `pkg/errs` | 分级错误 + HTTP 映射 | `Error{Kind, Code}` |
| `pkg/ptr` | 指针辅助（nullable 列扫描） | `To`, `From` |
| `pkg/tokenizer` | 预检计数，三级回退 | `Tokenizer` |
| `pkg/log` | slog 上下文封装 | `Logger` |
| `internal/config` | 配置加载（env + yaml） | `Config` |
| `internal/engine` | 引擎抽象：Adapter（一个）+ Profile（数据） | `Endpoint`, `Capability`, `Capacity`, `Profile`, `Adapter`, `Scraper` |
| `pkg/cost` | 成本池的时间积分与分摊，纯函数 | `Close`, `Sweep`, `Span`, `Integrate`, `Period` |
| `pkg/weights` | 权重格式分类，决定谁能加载 | `Format`, `Of` |
| `pkg/inventory` | operator → 控制面的上报契约（跨 module 共享，所以不能放 internal） | `Report`, `Cluster`, `Deployment` |
| `pkg/prom` | Prometheus 文本解析（只读四个 gauge，不引 client 库） | `Parse`, `Sample` |
| `internal/gateway/routing` | endpoint 选择（rendezvous + 健康） | `Picker`, `Rendezvous` |
| `internal/gateway/catalog` | 端点集合的活订阅：拉控制面 + 抓 load | `Refresher`, `Client` |
| `internal/gateway/transport` | SSE 透传 + usage tap | `Proxy`, `Tap` |
| `pkg/authn` | 凭据解析与主体（`internal/gateway/auth.go` 只是它的中间件接线） | `Principal`, `KeyStore`, `Limits` |
| `internal/gateway/ratelimit` | 预留-结算式分层限流 | `Limiter`, `Scope`, `Policies`, `Limited` |
| `internal/gateway/quota` | 预留-结算式分层预算（tenant + project 两层） | `Limiter`, `Reservation`, `Exceeded`, `Window` |
| `internal/gateway/handler` | OpenAI 端点 handler | `Chat`, `Samples` |
| `pkg/billing` | 计价算术与账本行类型 | `Rate`, `Price`, `Pricer`, `Record`, `Recorder` |
| `internal/store/postgres` | 领域仓储：租户、项目、密钥、限流读路径与计数器、价格本、账本、配额、成本池 | `DB`, `KeyStore`, `PolicySource`, `RateLimiter`, `Quota`, `PriceStore`, `Ledger`, `CostStore` |
| `internal/apiserver` | 控制台 REST API | — |

### `fleet-serving`（module `github.com/zlogic-labs/fleet-serving`，另一仓库）

| 包 | 职责 |
|---|---|
| `api/v1alpha1` | CRD Go 类型（controller-gen 生成 deepcopy） |
| `internal/controller` | 各 CRD 的 reconciler |
| `internal/report` | 周期上报 cluster inventory 与 deployment 状态到控制面（P7：只有 operator 知道 K8s） |
| `internal/render` | FleetDeployment → K8s 原生 Deployment + Service |
| `internal/scheduler` | GPU 装箱、拓扑匹配、gang 分配 |

它通过 `require github.com/zlogic-labs/fleet/core v…` 钉住主仓的一个版本，**不使用 `replace`**：本地联调把替换写进 `go.work`（已 gitignore），因为 `go.mod` 里的 `replace` 会跟着每个 `go get` 它的下游走，那是给库代码的禁忌。

## 5. 核心数据模型

### 5.1 引擎能力：Adapter 只有一个，Profile 是数据

```
core/pkg/engine/engine.go          Adapter 接口（按协议）· Capability · Capacity · Endpoint
core/pkg/engine/profile.go         Profile · Profiles · vllm / llama-cpp 两个字面量
core/pkg/engine/scrape.go          Scraper：一次 /metrics 同时读出 Load 与 Capacity
core/pkg/engine/openai/            唯一实现：probe 走 Profile 的候选列表
 core/pkg/cost/                      成本池：日历月、并发扫描线、时间积分、分摊
 core/pkg/weights/                 Format 分类（safetensors / gguf / unknown）
 core/pkg/inventory/               fleet-serving → 控制面的上报契约
 core/pkg/prom/                    Prometheus 文本解析
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
```

多集群不在 v1：第一版只有单集群。`FleetCluster` 不做成 CRD——集群级 inventory 由 fleet-serving 在集群内采集后 `PUT` 给控制面（`pkg/inventory.Report`），控制面与网关都不知道 K8s 的存在（P7）。真要多集群，加的是控制面的注册表，不是一个新 CRD。

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
| `rate_limit_policies` | ~~限流策略~~ —— **已取消**。限额是 `tenants` 与 `projects` 上的列，不是一张独立表：一个 scope 的限额和它的预算是同一个对象上的两列，分表只会让"这个租户总共能花多少"需要跨表才能回答 |

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

### 判据：谁拥有做决定所需的信息，这件事就归谁

| 归属 | 内容 | 依据 |
|---|---|---|
| **引擎内部** | prefix cache 命中、`gpu_memory_utilization`、block 分配、`cache_dtype`（fp8/nvfp4/int4）、`kv_offloading_backend`（目标是本实例 CPU）、chunked prefill、P/D 分离 | 依据 vLLM `vllm/config/cache.py`（main，2026-10-01）。`gpu_memory_utilization` 文档原话：*"It does not matter if you have another vLLM instance running on the same GPU... you can set it to 0.5 for each instance."* —— vLLM 自己就假设多实例共存，KV cache 是纯实例内概念。 |
| **网关** | 选哪个 replica（P3）、prefix 亲和路由 | 只有网关同时知道 token 计数、配额、成本池水位。 |

**网关的 prefix 亲和不搬运 KV cache。** 它是带偏置的负载均衡启发式，命中发生在被选中的引擎内部。

### 两个必须记住的坑

- **坑一（接 P8）**：prefix 亲和把流量钉死在一个副本，其他闲置，而闲置也在烧 GPU·小时。所以"命中率"可能拿"闲置率"换。**亲和必须有幅度上限**，超过就退回轮询。这个限幅参数属于网关，唯一存在理由是 P8。
- **坑二**：P/D 分离会削弱网关 prefix 亲和 —— 命中的是 P，收益体现在 D 侧，引擎内部已优化过。endpoint 声明 disaggregation 时网关应自动降权/关掉。

### 明确不做

不在网关做跨副本 KV 共享（会把网关变成分布式存储，多一个故障域）。真要跨实例走引擎 connector（LMCache remote / kv-transfer-config），Fleet 只声明配置。

### 容量数字从引擎读，不自己算

`engine.Capacity` 的 `KVTokens` / `MaxConcurrency` 来自引擎自报的 metrics，不来自 Fleet 的推算——只有引擎知道自己怎么切的 KV block，自己算就要重新实现一遍 vLLM 的 memory profiler，然后在下个版本漂移。

vLLM 把容量放在 **info gauge 的 label 上**（值恒为 1），不是放在指标值里：

```
vllm:cache_config_info{...,kv_cache_size_tokens="616000",kv_cache_max_concurrency="37.67",...} 1.0
```

`kv_cache_size_tokens` 是 "Per-DP-engine KV cache capacity in tokens (group-aware)"，`kv_cache_max_concurrency` 是 "Per-DP-engine maximum concurrency at max_model_len tokens"。未设置的字段 vLLM 渲染成字符串 `"None"`，所以解析时 `"None"` 必须当"没有"而不是当 0——否则会报出一个 0 token 的 KV cache，然后所有下游容量计算除以它。

llama-server **不报**任何 KV cache 容量等价物，所以 llama-cpp 的 `InfoGauge` 故意留空。

### 为什么不能轮询

vLLM 的 KV cache 按 prompt 前缀复用。轮询把前缀打散 → 命中率归零 → 同样的 GPU 吞吐掉一个数量级。llm-d 在 MI300X 上的实测：开启 prefix-cache 感知路由后输出 token/s 3x、TTFT 减半。

### 策略

```go
// key = 前 N rune 的 prompt 前缀，N 可配（默认 512）
// → Rendezvous hashing（不是一致性哈希环）
// 候选集内：打分 → 取最优
```

**用 rendezvous 而不是一致性哈希环**：fleet 是几十个端点不是几万个。rendezvous 在这个量级上分布完全均匀、不需要虚拟节点，而且**移除一个端点时只有落在它身上的 key 会迁移**——其他端点已经缓存的东西原封不动。环在端点增删时会造成大范围重映射，正好把 prefix cache 冲掉，也就是这个方案本来要保护的东西。

熔断按 `engine` + `endpoint` 两级计数，连续失败进入冷却。冷却期内不参与打分，但**健康探测继续**——避免"恢复后仍被标记不健康"的死锁。

**端点集合是活的**：`routing.Rendezvous` 带读写锁并支持 `Replace`，`gateway/catalog` 周期从控制面拉 deployment 状态刷新它。只读 `Available` 且有 address 的部署——`Scheduling` 是承诺，把流量发过去只会拿到 503，而那看起来像引擎繁忙。

**刷新失败时保留旧集合**，不清空。两种失败模式严重不对称：旧端点会把流量发给可能已经消失的引擎，而清空端点集合会把一次上报故障变成一次全面的推理故障。

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

### fleet-serving 依赖

`sigs.k8s.io/controller-runtime`、`k8s.io/{api,apimachinery,client-go}`、`sigs.k8s.io/yaml`

KubeRay / AIBrix 的 CRD **用 `unstructured` 消费**，不引它们的 Go client——它们的类型定义跟着上游 release 走，引进来就是版本地雷。

### 代码生成

| 工具 | 产出 | 仓库 |
|---|---|---|
| `sqlc` | `core/internal/store/postgres/*.sql.go` | fleet |
| `controller-gen` | `api/v1alpha1/zz_generated.deepcopy.go` + CRD yaml | fleet-serving |
| `goimports` | 格式化 | 两个 |

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
| 2 | auth + 限流 + 配额预留/结算 | 超限返回 429，预留正确回滚（已完成） |
| 3 | routing：一致性哈希 + 健康 | 同前缀命中同端点，熔断能恢复 |
| 4 | 计价 + 账本 + ClickHouse | 账实一致，对账任务能跑（计价与 PostgreSQL 账本已落地，ClickHouse 明细与对账任务尚未） |
| 5 | fleet-serving：CRD → K8s 原生 Deployment + Service | k3s 上 `kubectl apply` 能起一个引擎（已落地，`make e2e` 18 条断言） |
| 6 | 成本分摊 + 控制台 | 成本报表数字对得上 |
| 7 | autoscaling + 调度器 | 队列深度驱动扩缩，无抖动 |

阶段 1–4 全部不依赖 Kubernetes，可以纯 Go 单测覆盖。**这是把模块边界划在 P7 的直接收益。**

### 11.1 权重的分发策略不是一个全局开关

原来这条是"共享存储（GPFS/Lustre）vs 节点本地 NVMe + 预热"二选一。引入 `weights.Format` 之后**问题变了**：策略随格式走，所以不该是一个全局选择。

| 格式 | 典型体积 | 合理分发 |
|---|---|---|
| safetensors | DeepSeek-R1 671B = 1.3 TB | **只能**共享存储。复制到节点在物理上不成立。 |
| GGUF（Q4） | 0.5B–70B = 0.4–45 GB | 复制到节点本地 NVMe + 预热可行，冷启动从分钟级降到秒级。 |

也就是说：GGUF 部署的 autoscaler 冷启动可以做到可接受，safetensors 671B 部署的冷启动本质上是"加载 1.3 TB"的时间，扩缩容策略必须承认这个事实而不是假装可以预热。

这直接影响 P/D 分离是否可行，也影响 `FleetDeploymentSpec` 要不要显式声明 `WeightDelivery: shared | nodeLocal`。**倾向**：显式声明，operator 据此选 init container 还是直接挂载——但两条路径都还在 operator 里，尚未实现。

### 11.2 认证与限流：十四处不显然的取舍

阶段 2 已落地（`pkg/authn` + `internal/gateway/ratelimit`）。下面十四处决策看起来是小事，实际每一处都曾写错或差点写错，值得留下理由。

**限额绑在租户与 project 上，不绑在 key 上。** key 是凭据：会轮换、会泄漏，不是一个预算。绑上去只有两种结局——要么执行不了（租户再发一把 key 就绕过去了），要么限额跟着一个 secret 的寿命走，调低预算要先做一次轮换，而轮换会把调用方正在用的东西作废。所以 `Principal` 里有 `Tenant` 和 `Project`，没有 `RateLimit`；`api_keys` 表上也没有任何限额列。

**两个计数器，不是把两层取最紧合并成一个。** 这是本节最容易写错的一处。租户信封 300 rpm、项目限额 300 rpm，如果合并成一个桶，`acme` 建 10 个 project 就能拿到 3000——信封直接失效。**乘法陷阱**：凡是"子限额之和不得超过父限额"这种约束，只有一个计数器是守不住的，因为守不住的那一步是**加法**。所以每个请求都要独立校验两层：`Reserve` 在同一把锁里先判信封再判分区，任何一层拒绝就两层都不入账。

**分区被拒时不扣信封。** 反过来也一样重要：如果信封的扣减已经发生再判分区，一个"自己项目已满"的租户会为它**控制不了的**拒绝流量付费。而它恰恰是唯一一个没有手段自行缓解的请求量。

**key spec 里带限额要报错，不能忽略。** 旧的 `acme/team-a|rpm=600` 会被明确拒绝，错误信息里写明限额搬到了 `rate_limits.tenants` / `rate_limits.projects`。静默丢弃数字是最坏的一种兼容：运维以为有预算，其实在裸奔。

**声明的限额是"替换"服务端默认值，不是与之取大。** 零表示"没说"，该维度继承默认值（`Limits.OrDefaults`）。配置是运维写的，取大会让 `rpm=5` 被悄悄抬回 100——一份不按字面执行的配置比没有配置更糟。

**partition 上没说的维度是"不限制自己"，不是继承服务端默认。** 一个分区只在 `rpm` 维度有数字，意味着它在 token 维度**没有自己的上限**，而不是它继承了租户那个给"没人声明的租户"准备的默认值。两者写起来只差一个词，含义完全相反。

**project 限额高于所属租户信封时启动失败，不做钳制。** 它永远不可能生效，而写下它的人以为自己完成了一次预算切分。这跟 schema 里为什么没有这条 CHECK 约束是同一件事：它跨两张表，所以是应用层的约束，配置路径上由 `resolveLimits` 在启动时报出来。

**租户策略在首次请求时读一次并缓存**，所以运行中改配置不会改变一个已在服务中的 scope 的限额。代价是调低限额需要重启网关——写在这里是为了让它是一个决定而不是一个意外。

**`/healthz` 与 `/health` 不鉴权。** 不是"豁免中间件"，而是注册在鉴权子路由之外。K8s 探针拿不到凭据，一个返回 401 的 `/healthz` 会让 Pod 在服务完全正常时被判为失败——而运维对这种报警的反应是删掉探针，不是修探针。

**凭据在转发前剥掉。** 在 `authenticate` 里剥，不是在 proxy 里。等到 handler 忘了剥，租户 A 的 key 已经进了引擎 B 的请求日志，而那本日志可能属于另一个团队。发出去的东西收不回来。

**凭据本身是 `tenant/project/keyid` 整串，不是裸 key id。** 两个租户都可以把自己的 key 叫 `admin`；按裸 id 存的话后注册的会覆盖先注册的，而先注册那个租户的凭据会解析成**另一个租户**的 principal——这是跨租户读别人账本。顺带一个推论：裸的 `admin` 解析不出任何东西，401。

**认证关闭与认证失败是两件事。** `keyStore` 返回接口类型而不是 `*authn.Memory`：`return nil, nil` 装进接口字段会得到一个**非 nil 的接口持有 nil 指针**，于是 `store == nil` 为假，一个没配 key 的网关会因为"拥有一个什么都回答不了的 store"而拒绝所有请求。开发者笔记本和配错的生产必须不能是同一个现象。

**token 限额要算"预留 + 已结算"。** 结算会把一个请求的 token 从预留挪到已结算两栏里。只读预留那一栏的检查，会看到桶在每个请求结束后排空——真实流量里绝大多数请求在下一个到来之前就已完成，预留栏几乎是空的，于是**稳态短请求流永远碰不到上限**。`scripts/smoke.sh` 抓到过这个 bug：单测全都结算了远低于上限的数字，所以分不出两种读法。

配置：`FLEET_AUTH_REQUIRED`、`FLEET_API_KEYS`（分号分隔，`tenant/project/keyid`）、`FLEET_RATE_TENANTS`、`FLEET_RATE_PROJECTS`、`FLEET_RATE_RPM`、`FLEET_RATE_TPM`、`FLEET_DEFAULT_MAX_TOKENS`。启动时硬校验：要求鉴权却没给 key、不要求鉴权却给了 key、任何一条 key spec 解析失败或仍带限额、任何一条限额声明解析失败、project 限额高于其租户信封、同一 scope 声明两次——都拒绝启动。

接数据库后 `FLEET_API_KEYS` 就不再被读：key 存在 `api_keys` 表里，限流策略从 `tenants.request_limit` / `projects.request_limit` 读，价格从 `price_books` 读，账写 `usage_events`。`FLEET_DATABASE_URL` 设了就走数据库，不设全在内存——内存里没有账本，限流计数器也重启即失忆。配套的还有 `FLEET_DATABASE_MIGRATE`（启动时建表，`IF NOT EXISTS` 只保证"不存在才建"，不会把旧表改一致）和 `FLEET_DATABASE_PRICE_REFRESH`（默认 1 分钟，价格改了不用重启网关）。

配额已落地（`internal/gateway/quota`），见 §11.4。仍未实现：**周期计价与成本分摊**——预算是**滚动窗口**上的额度上限，成本池要按**日历月**折成金额，那是 §6 的 P8，两者是不同的窗口，不要混成一个开关。

**限流计数器不是账本，重启即失忆，这是刻意的。** 它是一道闸，不是账。而账本已经落地：`internal/store/postgres` 的 `Ledger` 只增不改，`billing.Record` 是它的行类型，`internal/billing` 是计价算术。两者分开是因为生命周期不同——闸可以丢，账不能。

**计数器有进程内的，也有落表的，取决于有没有数据库——而这个 bug 是静默的。** 最初的滑动窗口是 Go map 里的 60 个桶，`PolicySource` 只从数据库读**策略**、不读计数器。网关跑 N 个副本，有效 RPM 就是限额 × N，**永远如此**，而且**没有任何东西会报告异常**：每个副本单独看都是对的，只是加在一起超了。租户买了 100 rpm，拿到的是每副本 100。LiteLLM 在 [#40291](https://github.com/BerriAI/litellm/issues/40291) 上是同一个形状。

修法是把计数器也放进 Postgres（`rate_counters`，`RateLimiter`），语义与配额一致：租户行锁 + 窗口内求和。没有数据库时仍退回进程内实现——那是给笔记本的，`replicas > 1` 的部署不应该走到那条路。

两处细节值得留着：

- **不共用 `spend_counters`。** 那张表的桶按各条规则自己的窗口对齐，一条 5 小时窗口的规则会把限流器写的 1 秒桶一起求和进去，同一笔 token 被算两次。限流的窗口按定义就是固定 60 秒，1 秒桶是精确的而不是推算出来的，所以它是自己的一张表。
- **释放要回到当初扣的那一秒。** 进程内版本靠从当前秒往回走找到那一桶；能指出行的版本直接退款到自己被扣的那行，退款离开窗口的时刻才和扣款一样。

**并发测试的量必须远超限额，否则它证明不了任何事。** 第一个版本是 40 个 goroutine 对 50 的限额——**把行锁整个删掉它照样通过**，因为 40 < 50，一个坏掉的实现也无法超发。现在是 200 对 20，删掉 `FOR UPDATE` 会报 `granted 35 against a limit of 20`。

### 11.3 结算路径：为什么是这三步、这个顺序

阶段 4 的落账已经接到结算上（`internal/gateway/handler/settle.go`）。顺序本身是内容，不是实现细节：

**限流先结算。** 它是三步里唯一持锁的一步，而租户的额度应该在它的请求结束那一刻就还回去——一次慢的价格查询不该让租户等完才能继续花钱。

**计价第二，按引擎的 usage。** P6：引擎是 token 的唯一权威，所以价格查不到不会改变"花了多少"，只会改变"值多少"。

**落账最后。** 它是三步里唯一可能失败的失败，而到这一步已经没有什么可以失败的了。响应此时已经写完。

**响应写完之后才结算，而结算失败不影响响应。** 客户端已经拿到答案了，剩下的延迟是 Fleet 自己吸收的，上限 5 秒（`settleTimeout`）：够健康数据库用，又不至于让一个卡死的数据库把连接全占住——那会让**远多于一个请求**的流量失败。

**结算用的 context 从请求 context 上摘下来。** 客户端中途断开不该取消一笔已经计费的请求的落账。token 是真的花掉了，与乎调用方有没有等着听完没有关系。

**没有数据库 = 不记账，而不是报错。** `billingFor` 返回 nil，handler 把 nil 当"这套部署不记账"。笔记本和生产跑同一份代码，区别只有配置。

**没有价格的模型照样落账，落成 0，并打 error 日志。** 有两个理由，第二个才是重点：第一，token 已经花掉了，行必须存在，否则用量凭空消失；第二，**一个模型在服务流量却没有价格，等于白送客户 GPU，系统里没有别的东西会说出来**。把"没有价格"报成 0 正是本包要消灭的失败模式，所以 `Charge` 的 error 意思是"这个模型没有价格"，永远不是"这个模型免费"。

**账本记原始 token 数和拆开的 fresh/cached/reasoning，不记折算后的价。** 改定价规则时要能重跑历史，所以行里存的是事实和当时的价格本 id，`amounts_micro` 是那一笔真正收的数——报表不必去 join 一个可能已经改过的价格本。

### 11.4 配额：任意时长、任意维度

`internal/gateway/quota` + `budget_rules` + `spend_counters`。形状与 `ratelimit` 相同（预留→结算），但**不合并进 `ratelimit`**，原因是两个计数器的生命周期不同：

> **限流是一道闸，账本和预算是钱。** 限流重启即忘，最多丢一分钟的一次突发；预算重启即忘，就是让租户**每次重启都能把额度再花一遍**，永远。

这条曾被用来推出"限流可以是内存环形桶"，而那个推论是错的：进程内的计数器在 N 个副本下就是 N 倍限额，而它自己不报告任何问题——每个副本单独看都是对的。**重启即忘**只说明限流可以接受一个短窗口，它不说明这个窗口可以不出现在副本之间。两者现在都是表，见 §11.2 的第十四处。

**一条规则是四元组，不是两个数字。** `Rule{ScopeKind, ScopeID, Dimension, Limit, Window}`。最初 `tenants` 和 `projects` 上各有一个 `budget_units bigint`，看起来够用，直到它被这样一句话问倒：

> 5M tokens / 5 小时，外加 200 额度 / 30 天。

单个 bigint 只能表达一个维度、一个窗口。所以两张表各删掉那两列，换成一张 `budget_rules`，唯一索引 `(scope_kind, scope_id, dimension, window_seconds)`——**一个维度一个窗口只能有一条规则**，重复设是配置错误而不是覆盖。

**六个维度，因为 token 不是一个数。** `TokensTotal`、`TokensInput`、`TokensOutput`、`TokensCached`、`TokensFresh`、`Units`。`TokensFresh`（prompt 减 cached）是必须单列的那个：在前缀缓存命中率高的部署上，total prompt 可能比实际计费高一两个数量级，用 total tokens 做预算会在租户几乎没花钱的时候把他停掉。这条是**引擎内缓存**（§P1bis）直接漏到计费侧的一个后果，不是提前想到的。

**任意时长 ⇒ 不存在"清零点"，只能是桶上的求和。** 锚定窗口（自然月、每天）可以在边界清零，滚动窗口不行——"最近 5 小时"不是某个可以清零的计数器。所以 `spend_counters` 按 `bucket_start` 存行，读的时候在窗口内求和。`ResolutionFor(window)` 按窗口长度反推桶大小（1 分钟到 1 天，窗口越长桶越粗），**代价是检查精度只到每条边缘一个桶**：一个刚好在窗口边缘的桶会被整个算进来或整个不算。这是有意的取舍，写在代码里而不是藏起来——桶越细越准，但 30 天窗口按分钟切就是 43200 行/规则。

**`month = 30 天，不是日历月。** 滚动窗口不能有时 28 天有时 31 天，否则"每月 200 额度"这个说法对租户没有稳定含义。**日历月仍然需要，但它属于成本池**（§6 的 P8，发票周期），与预算窗口是两件事。`ParseDuration` 接受 Go duration 语法加 `d/w/mo/y` 别名；**裸数字按小时**，因为把 `5` 读成 5 分钟和读成 5 小时差 72 倍，而猜错的方向是烧掉真钱。

**计数按规则自己的 scope 存，不按请求的 scope。** 一条请求同时带着 tenant 和 project，两者可以各有规则。存成请求的 scope 会把信封和分区写进同一行，于是两者相加——租户总额被报成项目的。反过来，**tenant 规则只按 tenant 存**，它下面所有 project 的支出落在同一行，这不是优化，这是"信封"成为信封而不是每项目一个预算的全部原因。

**预留必须和求和在同一条语句里，不能先查再写。** 先查再写中间有缝，并发下每个请求都看见"还剩 1"然后都扣掉。`on conflict do update ... where` 的 `where` **只覆盖"行已存在"的情况**，于是每个 scope 的第一个请求走 INSERT 分支完全不被检查——建计数器的那个请求恰好是预算管不到的那一个。这条 INSERT 因此必须自己也带 `where`，两个分支问同一个问题：这一笔加得下吗。

**先锁 tenant 行，再在同一事务里检查。** 两处：锁必须在事务里而不是在池上（行锁属于持有它的 session，在池上锁一条连接然后在另一个事务里干活，等于锁了一条没人用的连接——比不锁更糟，因为它看起来像锁了）；锁一条而不是每规则一条（固定顺序取多把锁仍会死锁，两个持有对方锁的 scope）。锁 tenant 行是因为它必然存在，锁表方案需要"第一个请求建锁行"，而那是本次事务之外的一次写。

**结算释放的是预留额，不是实际额。** 两者在两条路径上都不同：预留时进 reserved 列，结算时把**当初预留的那个数**从 reserved 列拿掉、把**实际发生的数**加进 spent 列。把它们合成一个参数就是"预留永远不回来"或者"按错数退款"——而预留-结算的全部意义就是那个预留额必须回来，否则一次超量预留会永久吃掉租户的额度。变异测试确认这条被两个测试守住。

**拒绝返回 402 而不是 429。** 429 的语义是"马上重试"，客户端那么做是对的；402 的语义是"窗口结束后再来，或者加额度"，客户端紧循环重试 402 是在花钱证明自己不可能成功，而重试本身就在烧 GPU。新增 `errs.KindBudgetExhausted` 而不是复用 `KindRateLimited`，就是为了让客户端不看提示文字就能分支。预算库本身不可达时返回 503 而不是 402——那是故障，不是租户的问题。

**402 必须带 `Retry-After`。** 两类拒绝现在走同一个 `writeRefusal`。窗口空的时候"最早的非零桶"不存在，`whenFree` 回落到窗口末尾——**这仍然是真话**：容量确实会在窗口滑出后回来，而说"永远"会让客户端自己猜。

**账与预算必须写自同一个数字。** `TestTheBudgetAndTheLedgerAgree` 直接断言二者相等，而不是相信调用顺序。

**没有价格的模型按当前最低费率预留，不是预留 0。** 预算若对一个在服务流量的模型估 0 就等于完全不设防，而预算和账本会**一致地**声称"没花钱"，两者描述的都是一次真的用了 GPU 的请求。兜底值是**高估**，方向上让未定价模型**提前**停掉租户——那是可恢复的错误方向。完全没有价格本时仍可执行 token 规则，因为**计数 token 不需要价格**。

### 11.5 这一节的四条不变式

`internal/gateway/billing_test.go` 对着真 PostgreSQL 跑，断言的就是这四条：

1. 记录归到**鉴权认出的**租户/project/key，不是请求体里的任何东西；
2. 记的是**端点实际解析到的**模型，不是客户端写的字符串（否则一个别名指向便宜模型就能改写上个月的账单）；
3. token 用**引擎报的**，不是预留的上界（客户端要 4096 拿到 7 个，就只收 7 个）；
4. 没价格的模型、没报 usage 的引擎，行都在，且标得出来。

### 11.6 API 风格：为什么只有三条规则

整个控制面和网关的 `/fleet/status` 共用同一套形状，规则只有三条，其余都是从这三条推出来的：

1. **集合一律复数、置于顶层。** `/tenants`、`/projects`、`/keys`、`/budget-rules`、`/models`、`/pulls`、`/deployments`、`/clusters`。没有 `/tenant/{id}/projects` 这种嵌套——嵌套深一层，删除一个父节点时子节点的归属就要单独回答一次。
2. **条目的 id 允许含斜杠，所以条目路由以 `*` 结尾。** 租户是 `acme`，项目是 `acme/research`，预算是 `tenant/acme/tokens_total/5h`。chi 的 `{id}` 和 `{id:.+}` **都会在第一个斜杠停下**（实测三种 pattern 全部 404），`%2F` 编码形式却能通——于是同一份资源有两个地址，取决于客户端有没有编码。`*` 取整段再 unescape，两种写法落到同一行。
3. **动词不是路由。** 曾经的 `GET/POST /verify/{name}` 是一个"对资源做某个动作"的写法，现在它是 `GET /repositories/{name}?engine=…`——能不能被某个引擎加载是资源的一个属性，返回的就是资源本身。同理 `POST /deployments/{name}/scale` 变成 `PATCH /deployments/{name}`，`POST /operator/inventory` 变成 `PUT /inventory`（上报的是整个集群状态，报两次必须是幂等的，而不是两份）。

**没有数据库时，租户路由返回 400 而不是空列表。** 空列表读起来像"这个平台没有租户"，而真相是"你没接数据库"。让开发笔记本零配置可用，和让缺配置看起来像正常状态，是两件必须分开的事。

**三个 500 是真的不够。** 存储层区分 not found / conflict / invalid 三类，对应 404 / 409 / 400：唯一约束冲突和"我们坏了"给出的补救方向正好相反，对前者返回 500 会让客户端一直重试。同时 5xx 的原因只进日志不进响应体——`errs.Internal` 正是为此存在的，而原因必须**落到某个地方**，否则排查只能靠猜。

**迁移的边界画在"会不会重写数据"上，不在"会不会改表结构"。** `CREATE TABLE IF NOT EXISTS` 对已存在的表什么也不做，所以后来新增的列需要自己一条 `ALTER TABLE … ADD COLUMN IF NOT EXISTS`（本轮加 `api_keys.revoked_at` 就是）。会重写既有行（尤其是账本）的语句不放进 schema.sql，那需要真正的迁移工具和一条审过的 down 路径。

### 11.7 成本池：闲置容量必须落在某张账单上（P8）

P8 说计费是 GPU·小时的**固定成本分摊**，这句话有四条实现上的后果，每一条都和直觉相反。

**一、成本池按日历月，预算按滚动窗口。** `cost.Period` 是 UTC 日历月，`quota.ParseDuration` 里的 `1mo` 是 30 天。两者不是同一个开关：预算是一个人盯着烧钱速度看的速率，发票是别人按月付钱的周期。混成一个开关的后果是成本池每天漂一点，月底谁也说不清"这个月"指哪一段。

**二、Pool 全额分摊，Idle 单独报。** 这是一对容易搞反的加法：

```
Pool      = 池容量成本（全量）
Busy      = 请求真正占用的那部分
Idle      = Pool − Busy          ← P8 要的数：闲置率
Allocated = Pool                 ← 分摊给租户
```

`Busy + Idle = Pool`，`Allocated = Pool`，两个等式不同但都对。**只按 Busy 分摊会把闲置留在账外**，于是运营方默默吸收了一个利用率问题——而"让闲置可见"正是这个平台存在的理由。闲置不是可以摊掉的成本，它是有人做错了决定的证据。

**三、`1mo` 的月不是同一个月的月。** 预算里 `month = 30 天`，因为有时 28 天有时 31 天的窗口没人能算得清；成本池必须用真实日历月，否则跨月发票对不上。

**四、占用是并发积分，不是墙钟求和。** 这一条最反直觉，因为"每个请求 × 它的时长"看起来就是对的，而且单请求算出来的数一模一样。

```
十个并发请求跑十秒，共享一张卡：

墙钟求和   10 × 10s × 1 GPU = 100 GPU·秒   ← 错，是并发本身
扫描线     min(容量, 需求) × Δt  = 10 GPU·秒
```

差别不是舍入误差，**误差就是并发度**：它随负载增长，让一个已经打满的部署看起来比真正闲置的还忙，而且**给一个把流量串行化的租户发折扣**。这不是显示问题——错误的数字在被拿去和任何东西比较之前就已经错了，clamp 只会把它藏起来。所以 `cost.Sweep` 按每个时刻 `busy = min(capacity, demand)` 算，每个 key 拿 `busy × 自己的需求 / 总需求`，于是：

```
Σ key 的份额 = busy ≤ reserved
```

`perDeployment` 里的 `idle < 0` 钳制因此被删掉了——它当初正是在盖这个洞。现在 idle 为负就意味着这条不等式破了，那是故障而不是可以抹平的显示问题。

分摊同理：两个租户各自压满同一张卡时，谁也不能因为"请求更多"就多拿，因为池子只有那么大。这也是 `rate` 必须**在 busy 变化时整体重算**的原因——份额是"占忙碌时间的比例"，不是"占自己需求的比例"。`TestSweepSharesAreOfTheBusyTimeNotOfTheDemand` 盯的就是这个：zeta 的需求全程没变过，acme 走了之后它的份额从一半变成全部。

`Sweep` 还必须和 `Integrate` 用**同一个 `MaxGap`**。只在一处尊重它，会得到一个"reserved 一天、used 一个月"的部署，idle 一样是负的。

#### 让闲置可见的前提：容量必须有时间序列

`capacity_samples` 和 `deployment_samples` 记的是**报表到达时的样子**，而不是当前的样子。这是唯一让"我们有两个 GPU 持续了 3 小时"成为一句话的存储。没有它，成本池只能在 Fleet 开始观察的那一瞬间计算，任何一个自然月都算不出来。

写入有节流：值没变且距上一条不足 5 分钟就不写。operator 每 30 秒报一次，一个月 86,000 行；有了节流，一个从不变化的机群一个月只有几百行，而**变化立即落库**——否则一次 scale 事件会被抹平到它后面那段时间里，两分钟的突发容量被计费半小时。

`deployment_samples` 保留 `gpu_per_replica` 而不只是总数，因为请求是按**当时的形状**计价的。拿今天的形状去算上个月的账，等于让每次 scale 都悄悄重写一次历史账单。这里用 as-of 约束的 `LATERAL` 查询实现，`TestAPriceRequestIsPricedAgainstTheShapeItRanOn` 和 `TestARequestBeforeAnySampleCostsNothingRatherThanGuessing` 盯的就是它。

#### 覆盖率闸门

样本只覆盖了 40% 的月份，报出来的"月度成本"就是错的，而且看起来完全正常——这是个数字，它比真相小，没有人会从输出里看出来。所以默认拒绝关闭一个覆盖率低于 90% 的周期，错误信息里写明看到了多少。`?minCoverage=` 可以调低，但必须**说出来**，而且无论调没调低，报告里都记着实际算它的覆盖率。

没声明 GPU 单价的机群会得到一份 `priced: false` 的报告，`pool` 是 0，`poolGpuSeconds` 照常有值——**价格 Fleet 无从得知**（云账单、托管合同、自建折旧三样东西没有共同点），所以它是声明的。未定价不等于免费：调用方能同时看到"这个月有 744 GPU·小时"和"单价未定"，这两句合起来才是完整的事实。

## 12. 待定

- `[待定]` 是否第一版就支持 Anthropic 原生协议，还是只做 OpenAI 兼容 + 一个转换层
- `[待定]` 成本池的计价周期（自然月 vs 滚动窗口）与跨周期欠款处理
  — 周期已定为 **UTC 日历月**（见 §11.7，与预算的滚动窗口是两件事）。仍未定的是**跨周期欠款**：一个租户在 3 月用超了预算，4 月的池子该不该先补 3 月的窟窿，以及关停一个租户时未结的账怎么结。

## 13. 社区版与企业版

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
