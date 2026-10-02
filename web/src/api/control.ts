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
  cancel: (id: string) => request<void>(`${BASE}/pulls/${id}`, { method: 'DELETE' }),
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