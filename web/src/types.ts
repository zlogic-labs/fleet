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
  tokenizerId: string;
  contextLimit: number;
  createdAt: string;
  updatedAt: string;
  message?: string;
}

export interface PullJob {
  id: string;
  model: string;
  source: 'huggingface' | 'url';
  sourceRef: string;
  revision: string;
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
  age: string;
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
