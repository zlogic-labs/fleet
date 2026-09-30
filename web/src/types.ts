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
  expires_at?: string;
  version: string;
  capabilities: string[];
  endpoints: EndpointStatus[];
  recent: RecentSample[];
}

export interface EndpointStatus {
  model: string;
  base_url: string;
  replicas: number;
  healthy: boolean;
  queue_depth: number;
  running_requests: number;
  kv_cache_used: number;
}

export interface RecentSample {
  model: string;
  endpoint: string;
  streamed: boolean;
  ttft_ms: number;
  duration_ms: number;
  usage_known: boolean;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
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
  cost_export: 'Scheduled cost export',
};
