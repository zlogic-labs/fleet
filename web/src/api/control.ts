/** Calls against the control plane (fleet-apiserver). Every collection is plural; an item id may contain a slash and is escaped whole. */

import { request } from './client';
import type {
  ClusterStatus,
  Deployment,
  EngineProfile,
  PullJob,
  RegistryModel,
  StorageInfo,
  Tenant,
  Project,
  ApiKey,
  BudgetRule,
  CostRate,
  CostPeriodSummary,
  CostReport,
  OpenSpend,
  UsageAgreement,
} from '../types';

const BASE = '/api/v1';
const item = (path: string, id: string) => `${BASE}/${path}/${encodeURIComponent(id)}`;

export const models = {
  list: (signal?: AbortSignal) => request<RegistryModel[]>(`${BASE}/models`, { signal }),
  get: (name: string, signal?: AbortSignal) => request<RegistryModel>(item('models', name), { signal }),
  remove: (name: string) => request<void>(item('models', name), { method: 'DELETE' }),
  /**
   * Ask whether a repository is loadable by an engine. The engine is part of
   * the question, not a detail: a GGUF repository is complete for llama-cpp
   * and broken for vLLM.
   */
  usableBy: (name: string, engine: string) =>
    request<{ name: string; engine: string; objects: number; missing: string[]; usable: boolean }>(
      `${BASE}/repositories/${encodeURIComponent(name)}?engine=${encodeURIComponent(engine)}`,
    ),
};

export const engines = {
  list: (signal?: AbortSignal) => request<EngineProfile[]>(`${BASE}/engines`, { signal }),
};

export interface PullRequest {
  model: string;
  source: 'huggingface' | 'url';
  sourceRef: string;
  revision?: string;
  tokenizerId?: string;
  contextLimit?: number;
  hfToken?: string;
  engine?: string;
}

export const pulls = {
  start: (body: PullRequest) => request<PullJob>(`${BASE}/pulls`, { method: 'POST', body }),
  list: (signal?: AbortSignal) => request<PullJob[]>(`${BASE}/pulls`, { signal }),
  get: (id: string, signal?: AbortSignal) => request<PullJob>(`${BASE}/pulls/${id}`, { signal }),
  // Cancel is a state transition, not a deletion: the job stays readable so you
  // can see what happened to a download you stopped.
  cancel: (id: string) =>
    request<void>(`${BASE}/pulls/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ state: 'canceled' }),
    }),
};

export const storage = {
  get: (signal?: AbortSignal) => request<StorageInfo>(`${BASE}/storage`, { signal }),
};

export const cluster = {
  get: (signal?: AbortSignal) => request<ClusterStatus>(`${BASE}/clusters`, { signal }),
};

export const deployments = {
  list: (signal?: AbortSignal) => request<Deployment[]>(`${BASE}/deployments`, { signal }),
  scale: (name: string, replicas: number) =>
    request<void>(item('deployments', name), { method: 'PATCH', body: { replicas } }),
  remove: (name: string) => request<void>(item('deployments', name), { method: 'DELETE' }),
};

export const tenants = {
  list: (signal?: AbortSignal) => request<Tenant[]>(`${BASE}/tenants`, { signal }),
  get: (id: string, signal?: AbortSignal) => request<Tenant>(item('tenants', id), { signal }),
  create: (body: Partial<Tenant>) => request<void>(`${BASE}/tenants`, { method: 'POST', body }),
  update: (id: string, body: Partial<Tenant>) =>
    request<void>(item('tenants', id), { method: 'PATCH', body }),
  remove: (id: string) => request<void>(item('tenants', id), { method: 'DELETE' }),
};

export const projects = {
  list: (tenant?: string, signal?: AbortSignal) =>
    request<Project[]>(`${BASE}/projects${tenant ? `?tenant=${encodeURIComponent(tenant)}` : ''}`, {
      signal,
    }),
  create: (body: Partial<Project>) => request<void>(`${BASE}/projects`, { method: 'POST', body }),
  update: (id: string, body: Partial<Project>) =>
    request<void>(item('projects', id), { method: 'PATCH', body }),
  remove: (id: string) => request<void>(item('projects', id), { method: 'DELETE' }),
};

export const keys = {
  list: (project?: string, signal?: AbortSignal) =>
    request<ApiKey[]>(`${BASE}/keys${project ? `?project=${encodeURIComponent(project)}` : ''}`, {
      signal,
    }),
  create: (projectId: string, label: string) =>
    request<ApiKey & { secret: string }>(`${BASE}/keys`, {
      method: 'POST',
      body: { projectId, label },
    }),
  remove: (id: string) => request<void>(item('keys', id), { method: 'DELETE' }),
};

export const budgetRules = {
  list: (scopeId: string, signal?: AbortSignal) =>
    request<BudgetRule[]>(`${BASE}/budget-rules?scopeId=${encodeURIComponent(scopeId)}`, { signal }),
  save: (body: Omit<BudgetRule, 'windowSeconds'>) =>
    request<void>(`${BASE}/budget-rules`, { method: 'POST', body }),
  remove: (id: string) => request<void>(item('budget-rules', id), { method: 'DELETE' }),
};

/**
 * The cost pool. A rate is what a GPU-hour costs in a cluster — declared by an
 * operator because Fleet cannot know it, and applied when a period is closed.
 */
export const costRates = {
  list: (signal?: AbortSignal) => request<CostRate[]>(`${BASE}/cost-rates`, { signal }),
  save: (body: CostRate) => request<CostRate>(`${BASE}/cost-rates`, { method: 'POST', body }),
};

/**
 * Periods are closed, never edited. Closing is idempotent-but-exclusive: a
 * period that is already closed answers 409, because a second invoice that
 * silently overwrites the first is the failure mode invoicing has.
 */
export const costPeriods = {
  list: (signal?: AbortSignal) => request<CostPeriodSummary[]>(`${BASE}/cost-periods`, { signal }),
  get: (period: string, signal?: AbortSignal) =>
    request<CostReport>(item('cost-periods', period), { signal }),
  close: (period: string) =>
    request<CostReport>(item('cost-periods', period), { method: 'PUT' }),
};

/**
 * The calendar month still running: token charges so far, per scope and per
 * model.
 *
 * A separate collection, not a period. It has no pool and no idle share, because
 * this month's capacity is not a measurement yet — so do not expect a CostReport
 * shape here even though the two look similar.
 */
export const spend = {
  open: (signal?: AbortSignal) => request<OpenSpend>(`${BASE}/spend`, { signal }),
};

/**
 * Fleet's metering against itself: the engine's account of its output next to
 * the gateway's count of the text it forwarded.
 *
 * Read-only and fleet-wide rather than per tenant or per model, because it
 * describes the ledger rather than anything inside it.
 */
export const usageAgreement = {
  get: (signal?: AbortSignal) => request<UsageAgreement>(`${BASE}/usage-agreement`, { signal }),
};
