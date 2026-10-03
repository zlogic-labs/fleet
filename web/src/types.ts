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
   * means this gateway discovers deployments some other way — static upstreams,
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
  usageKnown: boolean;
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  estimated: boolean;
}

// ── control plane ───────────────────────────────────────────────

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
  /** Empty until the operator reports inventory. See docs/architecture.md §12. */
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

// ── the cost pool (P8) ─────────────────────────────────────────

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
   * Whole-number percentages, 0–100, rounded half up by the server.
   *
   * Not fractions in millionths — CostAllocation.share below is, and the two
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
  /** Allocated cost, micro-units. */
  amount: number;
  /** What the token ledger charged for the same traffic. */
  usageMicro: number;
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
  coveragePercent: number;
  poolGpuSeconds: number;
  notes?: string[];
  tenants: CostAllocation[];
  deployments: CostDeployment[];
}

export interface OpenSpendScope {
  /** "tenant/project", or bare "tenant" when the request carried no project. */
  id: string;
  tenant?: string;
  project?: string;
  unitsMicro: number;
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
 * measurement yet. `priced` says whether a GPU-hour rate is declared — the
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
  requests: number;
  estimated: number;
  priced: boolean;
}
