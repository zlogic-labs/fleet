/** Shapes returned by the gateway and the control plane. */

export interface Model {
  id: string;
  object?: string;
  created?: number;
  owned_by?: string;
}

/** GET /fleet/status */
export interface FleetStatus {
  edition: 'community' | 'enterprise';
  customer?: string;
  expiresAt?: string;
  version: string;
  /**
   * Where this gateway reaches the control plane, when it was told. Absent
   * means this gateway discovers deployments some other way 鈥?static upstreams,
   * or a control plane on the same origin as the console.
   */
  controlPlane?: string;
  capabilities: string[];
  endpoints: EndpointStatus[];
  recent: RecentSample[];
}

export interface EndpointStatus {
  model: string;
  baseUrl: string;
  replicas: number;
  healthy: boolean;
  queueDepth: number;
  runningRequests: number;
  kvCacheUsed: number;
}

export interface RecentSample {
  tenant?: string;
  project?: string;
  model: string;
  endpoint: string;
  streamed: boolean;
  ttftMs: number;
  durationMs: number;
  /** durationMs minus ttftMs: the part that produced the answer. */
  decodeMs: number;
  /**
   * How long the engine held the request before decoding it, from the engine's
   * own per-request metrics. Absent 鈥?not zero 鈥?when the engine publishes none,
   * which is the ordinary case: vLLM needs `--enable-per-request-metrics` and
   * llama-server does not separate queueing from prompt evaluation at all.
   */
  queueMs?: number;
  usageKnown: boolean;
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  estimated: boolean;
}

// 鈹€鈹€ control plane 鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€

/**
 * A model in the registry, as distinct from a Model the gateway serves.
 * A registry entry has storage and a lifecycle; a served model has replicas.
 */
export interface RegistryModel {
  name: string;
  source: 'huggingface' | 'url';
  sourceRef: string;
  revision: string;
  storagePrefix: string;
  sizeBytes: number;
  files: number;
  state: 'pending' | 'pulling' | 'ready' | 'failed';
  /**
   * How the weights are laid out, inferred from the files the repository
   * actually had. It decides which engines can load the model, so it is shown
   * next to the name rather than buried in a details panel.
   */
  format: 'safetensors' | 'gguf' | 'unknown';
  /** Empty for GGUF, where the tokenizer is inside the weights file. */
  tokenizerId: string;
  contextLimit: number;
  createdAt: string;
  updatedAt: string;
  message?: string;
}

/**
 * One engine family Fleet knows how to serve a model with. This is a
 * projection of a server-side profile, not something the browser decides:
 * whether llama-cpp can load a GGUF model is answered by the control plane
 * and merely displayed here.
 */
export interface EngineProfile {
  name: string;
  format: string;
  minCompute: number;
  minComputeLabel: string;
  requiresGpu: boolean;
  metrics: boolean;
  tokenize: boolean;
  /** Whether the engine reports how long it held a request before decoding it. */
  queueTime: boolean;
  /** The engine-specific caveat: which flag turns it on, or why it cannot. */
  requestTimingsNote: string;
  /** The full path Fleet reads, container included, e.g. "metrics.queue_time_ms". */
  queueTimeField: string;
  knownModels: string[];
  notes: string;
}

export interface PullJob {
  id: string;
  model: string;
  source: 'huggingface' | 'url';
  sourceRef: string;
  revision: string;
  format: string;
  state: 'queued' | 'running' | 'done' | 'failed' | 'canceled';
  progress: number;
  bytesDone: number;
  bytesTotal: number;
  filesDone: number;
  filesTotal: number;
  currentFile: string;
  error?: string;
  startedAt: string;
  finishedAt?: string;
}

export interface StorageInfo {
  endpoint: string;
  bucket: string;
  region: string;
  reachable: boolean;
  usedBytes: number;
  objectCount: number;
  message?: string;
}

export interface GpuSummary {
  model: string;
  count: number;
  totalMemoryMiB: number;
}

export interface ClusterNode {
  name: string;
  ready: boolean;
  roles: string[];
  gpu: GpuSummary;
  allocatableMemoryMiB: number;
  kubeletVersion: string;
  osImage: string;
  addresses: Record<string, string>;
  reportedAt: string;
}

export interface ClusterStatus {
  /** Empty until the operator reports inventory. See docs/architecture.md 搂12. */
  clusters: ClusterReport[];
}

export interface ClusterReport {
  name: string;
  reachable: boolean;
  version: string;
  nodeCount: number;
  readyNodes: number;
  gpuCount: number;
  readyGpus: number;
  cpuMillicores: number;
  memoryMiB: number;
  nodes: ClusterNode[];
  reportedAt?: string;
  message?: string;
}

export interface Deployment {
  name: string;
  namespace: string;
  model: string;
  desiredReplicas: number;
  readyReplicas: number;
  tensorParallelSize: number;
  pipelineParallelSize: number;
  gpuPerReplica: number;
  state: 'Pending' | 'Scheduling' | 'Progressing' | 'Available' | 'Degraded' | 'Failed';
  reason?: string;
  updatedAt: string;
}

export const CAPABILITIES: Record<string, string> = {
  sso: 'SSO / directory login',
  audit: 'Immutable audit trail',
  rbac: 'Per-role permissions',
  policy: 'Request policy engine',
  multicluster: 'Multi-cluster control',
  ha: 'Replicated control plane',
  costExport: 'Scheduled cost export',
};

export interface Tenant {
  id: string;
  name: string;
  requestLimit: number;
  tokenLimit: number;
  active: boolean;
  createdAt: number;
  projects?: Project[];
}

export interface Project {
  id: string;
  tenantId: string;
  name: string;
  requestLimit: number;
  tokenLimit: number;
  createdAt: number;
}

export interface ApiKey {
  id: string;
  tenantId: string;
  projectId: string;
  label: string;
  prefix: string;
  createdAt: number;
  revokedAt?: number;
}

export type BudgetDimension =
  | 'tokens_total'
  | 'tokens_input'
  | 'tokens_output'
  | 'tokens_cached'
  | 'tokens_fresh'
  | 'units';

export interface BudgetRule {
  scopeKind: 'tenant' | 'project';
  scopeId: string;
  dimension: BudgetDimension;
  limit: number;
  /** Serialised Go duration or Fleet alias: "5h", "1mo", "1w". */
  window: string;
  windowText?: string;
  windowSeconds?: number;
  resolutionSeconds?: number;
}

// 鈹€鈹€ the cost pool (P8) 鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€

/** What a GPU-hour costs in one cluster. Declared, never inferred. */
export interface CostRate {
  cluster: string;
  /** Millionths of a currency unit. */
  gpuHourMicro: number;
  currency: string;
}

export interface CostPeriodSummary {
  period: string;
  currency: string;
  priced: boolean;
  pool: number;
  idle: number;
  /**
   * Whole-number percentages, 0鈥?00, rounded half up by the server.
   *
   * Not fractions in millionths 鈥?CostAllocation.share below is, and the two
   * being different is a trap. `humanPct` expects the millionths scale; using
   * it here printed 100 as 0.01%.
   */
  idlePercent: number;
  coveragePercent: number;
  poolGpuSeconds: number;
  closedAt?: string;
}

export interface CostAllocation {
  key: string;
  /** A fraction of the pool in millionths. */
  share: number;
  gpuSeconds: number;
  /** Share of the GPU-hour pool, micro-units. Zero for direct-only scopes. */
  amount: number;
  /** The part of amount collected on behalf of a corrected earlier period. */
  adjustment: number;
  /** What the token ledger charged for the same traffic. */
  usageMicro: number;
  /**
   * Paid to a vendor for the same traffic, micro-units.
   *
   * Kept out of `amount` on purpose. A pool share is eight dollars because other
   * people's money is in the pool; a vendor bill is the invoice. Adding them
   * produces a number that means neither.
   */
  direct: number;
}

export interface CostDeployment {
  name: string;
  reservedGpuSeconds: number;
  usedGpuSeconds: number;
  idleGpuSeconds: number;
  idlePercent: number;
}

export interface CostReport {
  period: string;
  from: string;
  to: string;
  currency: string;
  priced: boolean;
  pool: number;
  busy: number;
  idle: number;
  idlePercent: number;
  allocated: number;
  /**
   * Vendor charges for the same traffic, micro-units.
   *
   * Never folded into `allocated`. The two are different cost centres: one is
   * this fleet's own capacity, already paid for; the other is an invoice from
   * someone else. `allocated === pool` holds independently of this number.
   */
  direct: number;
  providers: CostProviderSpend[];
  /** 1 unless the period was recomputed after later data arrived. */
  revision: number;
  coveragePercent: number;
  poolGpuSeconds: number;
  /** Corrections carried in from earlier periods, and their net. */
  adjustments: CostAdjustment[];
  adjustmentTotal: number;
  /** The movements this period's revision caused, and who collects them. */
  amended: CostAdjustment[];
  notes?: string[];
  tenants: CostAllocation[];
  deployments: CostDeployment[];
}

/**
 * One price book: what a model costs per million tokens from one provider.
 *
 * Keyed by model *and* provider, which is the point. The same weights from this
 * fleet's own pool are a share of a fixed cost; from a vendor they are billed at
 * the vendor's list price, three orders of magnitude apart. One book per model
 * would let whichever was declared last price both.
 */
export interface PriceBook {
  /** Quoted back by the ledger on every row it priced. */
  id: string;
  /** The resolved model 鈥?what the endpoint serves, not what the client typed. */
  model: string;
  /** Lowercased vendor name. Empty means this fleet's own capacity. */
  provider: string;
  /** Quota units per million fresh prompt tokens. */
  input: number;
  /** Quota units per million generated tokens. */
  output: number;
  /** Quota units per million prompt tokens served from the prefix cache. */
  cached: number;
  /** Zero means "charge reasoning at the output rate", which is the usual case. */
  reasoning?: number;
  /** When this book took force. May be in the future for an announced rise. */
  effectiveFrom: string;
}

export type PriceBookInput = Omit<PriceBook, 'id' | 'effectiveFrom'> & {
  effectiveFrom?: string;
};

export interface CostProviderSpend {
  /** Lowercased vendor name, empty for this fleet's own capacity. */
  provider: string;
  amount: number;
  requests: number;
}

export interface CostAdjustment {
  scope: string;
  /** The period on the other end: corrected for amendments, collecting for adjustments. */
  forPeriod: string;
  gpuSeconds: number;
  amount: number;
}

/**
 * Fleet's own metering, audited against itself.
 *
 * `sources` counts settled rows by what they were billed on: `engine` is
 * measured, `counted` is measured by the gateway, `reserved` is arithmetic.
 */
export interface UsageAgreement {
  period: string;
  from: string;
  to: string;
  asOf: string;
  sources: Record<'engine' | 'counted' | 'reserved', number> & Record<string, number>;
  keys: UsageAgreementKey[];
  /** How many keys are worth stopping for. */
  faults: number;
  minSamples: number;
  tolerancePercent: number;
}

export interface UsageAgreementKey {
  endpoint: string;
  /** counted / engine. Zero when there is nothing to compare. */
  ratio: number;
  /** Signed whole percent. The whole engine figure is 0. */
  percent: number;
  fault: boolean;
  reason: string;
  engineRows: number;
  countedRows: number;
  truncatedRows: number;
}

export interface OpenSpendScope {
  /** "tenant/project", or bare "tenant" when the request carried no project. */
  id: string;
  tenant?: string;
  project?: string;
  unitsMicro: number;
  /** The vendor-charged part of unitsMicro. See OpenSpend.poolMicro. */
  directMicro: number;
  requests: number;
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  estimated: number;
}

export interface OpenSpendModel {
  model: string;
  unitsMicro: number;
  requests: number;
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  estimatedRequests: number;
}

/**
 * The calendar month still running.
 *
 * Not a CostReport and deliberately shaped unlike one: there is no pool, no
 * allocation and no idle share, because this month's capacity is not a
 * measurement yet. `priced` says whether a GPU-hour rate is declared 鈥?the
 * figures below are token charges and exist either way.
 */
export interface OpenSpend {
  period: string;
  from: string;
  to: string;
  asOf: string;
  scopes: OpenSpendScope[];
  models: OpenSpendModel[];
  totalsMicro: number;
  /** Token charges against this fleet's own capacity. */
  poolMicro: number;
  /** Token charges an external vendor billed. */
  directMicro: number;
  /** Per vendor, for the line items an operator will reconcile by hand. */
  providers: CostProviderSpend[];
  requests: number;
  estimated: number;
  priced: boolean;
}

/**
 * What one store says about a window of usage records.
 *
 * Records is a count and the rest are sums, which is what lets a comparison
 * tell "a row is missing" apart from "a row is wrong": the first moves the
 * count, the second does not.
 */
export interface ReconciliationTally {
  records: number;
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  amountMicro: number;
}

export interface ReconciliationDifference {
  field: string;
  ledger: number;
  replica: number;
}

export interface ReconciliationGroup {
  /** "tenant/model", the same spelling on both sides of the comparison. */
  key: string;
  reason: string;
  ledger: ReconciliationTally;
  replica: ReconciliationTally;
  fields: ReconciliationDifference[];
}

/**
 * Whether the reporting copy holds what the books hold.
 *
 * `available` false is a supported deployment rather than an error: without a
 * detail store there is nothing to compare against, and `note` says which of the
 * two reasons it is. The ledger side is still reported, because that is the copy
 * which cannot be regenerated.
 */
export interface UsageReconciliation {
  period: string;
  from: string;
  to: string;
  asOf: string;
  /** How far the window stops short of now. Non-zero only for a running month. */
  lagSeconds: number;
  available: boolean;
  note?: string;
  groupLimit: number;
  ledger: ReconciliationTally;
  replica: ReconciliationTally;
  agreed: boolean;
  fields: ReconciliationDifference[];
  groups: ReconciliationGroup[];
  omitted: number;
}